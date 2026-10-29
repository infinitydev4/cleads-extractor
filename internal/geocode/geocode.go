// Package geocode resolves a free-text location ("Lyon", "Paris 11e",
// "Bouches-du-Rhône") into a bounding box the planner can tile.
package geocode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/source"
	"golang.org/x/time/rate"
)

var ErrNotFound = errors.New("location not found")

// Geocoder resolves a location to a bounding box.
type Geocoder interface {
	Geocode(ctx context.Context, query, country, lang string) (domain.BBox, error)
}

// Nominatim uses OpenStreetMap's geocoder. The public instance allows 1 req/s
// and requires an identifying User-Agent; results are cached.
type Nominatim struct {
	Endpoint  string
	UserAgent string
	Client    *http.Client
	limiter   *rate.Limiter
}

func NewNominatim(endpoint, ua string) *Nominatim {
	if endpoint == "" {
		endpoint = "https://nominatim.openstreetmap.org/search"
	}
	if ua == "" {
		ua = "CLEADS-Business-Extractor/1.0"
	}
	return &Nominatim{
		Endpoint: endpoint, UserAgent: ua,
		Client:  source.NewHTTPClient(10*time.Second, 2, nil),
		limiter: rate.NewLimiter(rate.Every(1100*time.Millisecond), 1),
	}
}

func (n *Nominatim) Geocode(ctx context.Context, query, country, lang string) (domain.BBox, error) {
	if err := n.limiter.Wait(ctx); err != nil {
		return domain.BBox{}, err
	}
	q := url.Values{"q": {query}, "format": {"jsonv2"}, "limit": {"1"}}
	if country != "" {
		q.Set("countrycodes", strings.ToLower(country))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.Endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return domain.BBox{}, err
	}
	req.Header.Set("User-Agent", n.UserAgent)
	if lang != "" {
		req.Header.Set("Accept-Language", lang)
	}
	resp, err := n.Client.Do(req)
	if err != nil {
		return domain.BBox{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return domain.BBox{}, source.ClassifyHTTP(resp, body)
	}
	var out []struct {
		BoundingBox []string `json:"boundingbox"` // [minLat, maxLat, minLon, maxLon]
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return domain.BBox{}, fmt.Errorf("nominatim decode: %w", err)
	}
	if len(out) == 0 || len(out[0].BoundingBox) != 4 {
		return domain.BBox{}, fmt.Errorf("%w: %q", ErrNotFound, query)
	}
	f := func(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }
	bb := out[0].BoundingBox
	box := domain.BBox{MinLat: f(bb[0]), MaxLat: f(bb[1]), MinLng: f(bb[2]), MaxLng: f(bb[3])}
	if !box.Valid() {
		return domain.BBox{}, fmt.Errorf("%w: invalid bbox for %q", ErrNotFound, query)
	}
	return box, nil
}

// Google uses the Geocoding API (viewport of the best result).
type Google struct {
	APIKey   string
	Endpoint string
	Client   *http.Client
}

func NewGoogle(key string) *Google {
	return &Google{APIKey: key, Endpoint: "https://maps.googleapis.com/maps/api/geocode/json",
		Client: source.NewHTTPClient(10*time.Second, 4, nil)}
}

func (g *Google) Geocode(ctx context.Context, query, country, lang string) (domain.BBox, error) {
	q := url.Values{"address": {query}, "key": {g.APIKey}}
	if country != "" {
		q.Set("region", strings.ToLower(country))
		q.Set("components", "country:"+strings.ToUpper(country))
	}
	if lang != "" {
		q.Set("language", lang)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.Endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return domain.BBox{}, err
	}
	resp, err := g.Client.Do(req)
	if err != nil {
		return domain.BBox{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return domain.BBox{}, source.ClassifyHTTP(resp, body)
	}
	var out struct {
		Status  string `json:"status"`
		Results []struct {
			Geometry struct {
				Viewport struct {
					Northeast struct{ Lat, Lng float64 } `json:"northeast"`
					Southwest struct{ Lat, Lng float64 } `json:"southwest"`
				} `json:"viewport"`
				Bounds *struct {
					Northeast struct{ Lat, Lng float64 } `json:"northeast"`
					Southwest struct{ Lat, Lng float64 } `json:"southwest"`
				} `json:"bounds"`
			} `json:"geometry"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return domain.BBox{}, err
	}
	if out.Status != "OK" || len(out.Results) == 0 {
		return domain.BBox{}, fmt.Errorf("%w: %q (%s)", ErrNotFound, query, out.Status)
	}
	g0 := out.Results[0].Geometry
	ne, sw := g0.Viewport.Northeast, g0.Viewport.Southwest
	if g0.Bounds != nil {
		ne, sw = g0.Bounds.Northeast, g0.Bounds.Southwest
	}
	return domain.BBox{MinLat: sw.Lat, MinLng: sw.Lng, MaxLat: ne.Lat, MaxLng: ne.Lng}, nil
}

// Cached wraps a Geocoder with an in-memory TTL cache.
type Cached struct {
	Inner Geocoder
	TTL   time.Duration
	mu    sync.Mutex
	m     map[string]cacheEntry
}

type cacheEntry struct {
	box domain.BBox
	exp time.Time
}

func NewCached(inner Geocoder, ttl time.Duration) *Cached {
	return &Cached{Inner: inner, TTL: ttl, m: make(map[string]cacheEntry)}
}

func (c *Cached) Geocode(ctx context.Context, query, country, lang string) (domain.BBox, error) {
	k := strings.ToLower(query) + "|" + country
	c.mu.Lock()
	if e, ok := c.m[k]; ok && time.Now().Before(e.exp) {
		c.mu.Unlock()
		return e.box, nil
	}
	c.mu.Unlock()
	box, err := c.Inner.Geocode(ctx, query, country, lang)
	if err != nil {
		return box, err
	}
	c.mu.Lock()
	if len(c.m) > 10_000 {
		clear(c.m)
	}
	c.m[k] = cacheEntry{box: box, exp: time.Now().Add(c.TTL)}
	c.mu.Unlock()
	return box, nil
}

// Static resolves from a fixed table; used in tests and benchmarks.
type Static map[string]domain.BBox

func (s Static) Geocode(_ context.Context, query, _, _ string) (domain.BBox, error) {
	if b, ok := s[strings.ToLower(query)]; ok {
		return b, nil
	}
	return domain.BBox{}, fmt.Errorf("%w: %q", ErrNotFound, query)
}
