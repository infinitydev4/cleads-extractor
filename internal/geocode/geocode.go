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

// Result is a geocoded area.
type Result struct {
	BBox    domain.BBox
	Country string // ISO 3166-1 alpha-2 of the match, when known
}

// Geocoder resolves a location to a bounding box. An empty country searches
// worldwide.
type Geocoder interface {
	Geocode(ctx context.Context, query, country, lang string) (Result, error)
	// CountryAt returns the ISO country code at a point ("" if unknown).
	CountryAt(ctx context.Context, p domain.Point) (string, error)
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

func (n *Nominatim) Geocode(ctx context.Context, query, country, lang string) (Result, error) {
	if err := n.limiter.Wait(ctx); err != nil {
		return Result{}, err
	}
	q := url.Values{"q": {query}, "format": {"jsonv2"}, "limit": {"1"}, "addressdetails": {"1"}}
	if country != "" {
		q.Set("countrycodes", strings.ToLower(country))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.Endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("User-Agent", n.UserAgent)
	if lang != "" {
		req.Header.Set("Accept-Language", lang)
	}
	resp, err := n.Client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return Result{}, source.ClassifyHTTP(resp, body)
	}
	var out []struct {
		BoundingBox []string `json:"boundingbox"` // [minLat, maxLat, minLon, maxLon]
		Address     struct {
			CountryCode string `json:"country_code"`
		} `json:"address"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return Result{}, fmt.Errorf("nominatim decode: %w", err)
	}
	if len(out) == 0 || len(out[0].BoundingBox) != 4 {
		return Result{}, fmt.Errorf("%w: %q", ErrNotFound, query)
	}
	f := func(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }
	bb := out[0].BoundingBox
	box := domain.BBox{MinLat: f(bb[0]), MaxLat: f(bb[1]), MinLng: f(bb[2]), MaxLng: f(bb[3])}
	if !box.Valid() {
		return Result{}, fmt.Errorf("%w: invalid bbox for %q", ErrNotFound, query)
	}
	return Result{BBox: box, Country: strings.ToUpper(out[0].Address.CountryCode)}, nil
}

func (n *Nominatim) CountryAt(ctx context.Context, p domain.Point) (string, error) {
	if err := n.limiter.Wait(ctx); err != nil {
		return "", err
	}
	reverse := strings.Replace(n.Endpoint, "/search", "/reverse", 1)
	q := url.Values{"lat": {strconv.FormatFloat(p.Lat, 'f', 6, 64)}, "lon": {strconv.FormatFloat(p.Lng, 'f', 6, 64)},
		"format": {"jsonv2"}, "zoom": {"3"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reverse+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", n.UserAgent)
	resp, err := n.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Address struct {
			CountryCode string `json:"country_code"`
		} `json:"address"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", err
	}
	return strings.ToUpper(out.Address.CountryCode), nil
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

func (g *Google) Geocode(ctx context.Context, query, country, lang string) (Result, error) {
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
		return Result{}, err
	}
	resp, err := g.Client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return Result{}, source.ClassifyHTTP(resp, body)
	}
	var out struct {
		Status  string `json:"status"`
		Results []struct {
			AddressComponents []struct {
				ShortName string   `json:"short_name"`
				Types     []string `json:"types"`
			} `json:"address_components"`
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
		return Result{}, err
	}
	if out.Status != "OK" || len(out.Results) == 0 {
		return Result{}, fmt.Errorf("%w: %q (%s)", ErrNotFound, query, out.Status)
	}
	g0 := out.Results[0].Geometry
	ne, sw := g0.Viewport.Northeast, g0.Viewport.Southwest
	if g0.Bounds != nil {
		ne, sw = g0.Bounds.Northeast, g0.Bounds.Southwest
	}
	res := Result{BBox: domain.BBox{MinLat: sw.Lat, MinLng: sw.Lng, MaxLat: ne.Lat, MaxLng: ne.Lng}}
	for _, c := range out.Results[0].AddressComponents {
		for _, t := range c.Types {
			if t == "country" {
				res.Country = c.ShortName
			}
		}
	}
	return res, nil
}

func (g *Google) CountryAt(ctx context.Context, p domain.Point) (string, error) {
	q := url.Values{"latlng": {fmt.Sprintf("%.6f,%.6f", p.Lat, p.Lng)}, "result_type": {"country"}, "key": {g.APIKey}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.Endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	resp, err := g.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Results []struct {
			AddressComponents []struct {
				ShortName string   `json:"short_name"`
				Types     []string `json:"types"`
			} `json:"address_components"`
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", err
	}
	for _, r := range out.Results {
		for _, c := range r.AddressComponents {
			for _, t := range c.Types {
				if t == "country" {
					return c.ShortName, nil
				}
			}
		}
	}
	return "", nil
}

// Cached wraps a Geocoder with an in-memory TTL cache.
type Cached struct {
	Inner Geocoder
	TTL   time.Duration
	mu    sync.Mutex
	m     map[string]cacheEntry
}

type cacheEntry struct {
	res Result
	exp time.Time
}

func NewCached(inner Geocoder, ttl time.Duration) *Cached {
	return &Cached{Inner: inner, TTL: ttl, m: make(map[string]cacheEntry)}
}

func (c *Cached) Geocode(ctx context.Context, query, country, lang string) (Result, error) {
	k := strings.ToLower(query) + "|" + country
	c.mu.Lock()
	if e, ok := c.m[k]; ok && time.Now().Before(e.exp) {
		c.mu.Unlock()
		return e.res, nil
	}
	c.mu.Unlock()
	res, err := c.Inner.Geocode(ctx, query, country, lang)
	if err != nil {
		return res, err
	}
	c.mu.Lock()
	if len(c.m) > 10_000 {
		clear(c.m)
	}
	c.m[k] = cacheEntry{res: res, exp: time.Now().Add(c.TTL)}
	c.mu.Unlock()
	return res, nil
}

func (c *Cached) CountryAt(ctx context.Context, p domain.Point) (string, error) {
	// Country borders do not move: cache on a ~10 km grid.
	k := fmt.Sprintf("@%.1f,%.1f", p.Lat, p.Lng)
	c.mu.Lock()
	if e, ok := c.m[k]; ok && time.Now().Before(e.exp) {
		c.mu.Unlock()
		return e.res.Country, nil
	}
	c.mu.Unlock()
	cc, err := c.Inner.CountryAt(ctx, p)
	if err != nil || cc == "" {
		return cc, err
	}
	c.mu.Lock()
	c.m[k] = cacheEntry{res: Result{Country: cc}, exp: time.Now().Add(c.TTL)}
	c.mu.Unlock()
	return cc, nil
}

// Static resolves from a fixed table; used in tests and benchmarks.
type Static map[string]domain.BBox

func (s Static) Geocode(_ context.Context, query, _, _ string) (Result, error) {
	if b, ok := s[strings.ToLower(query)]; ok {
		return Result{BBox: b}, nil
	}
	return Result{}, fmt.Errorf("%w: %q", ErrNotFound, query)
}

func (s Static) CountryAt(context.Context, domain.Point) (string, error) { return "", nil }
