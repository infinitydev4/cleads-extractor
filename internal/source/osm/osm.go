// Package osm queries OpenStreetMap through the Overpass API. Data is
// licensed ODbL: it can be stored and reused, which makes it a foundation for
// an independent prospecting database (attribution required on exports).
package osm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/source"
)

const (
	Name      = "osm"
	userAgent = "CLEADS-Business-Extractor/1.0 (+https://cleads.fr)"
)

// Public Overpass instances. The main one is often at capacity; mirrors are
// tried in turn. For volume, run a private instance and set OVERPASS_URL.
var defaultEndpoints = []string{
	"https://overpass-api.de/api/interpreter",
	"https://overpass.private.coffee/api/interpreter",
	"https://maps.mail.ru/osm/tools/overpass/api/interpreter",
}

type Config struct {
	Endpoints   []string
	HTTPClient  *http.Client
	RatePerSec  float64
	Concurrency int
}

type Source struct {
	cfg  Config
	next atomic.Uint32
}

func New(cfg Config) *Source {
	if len(cfg.Endpoints) == 0 {
		cfg.Endpoints = defaultEndpoints
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = source.NewHTTPClient(90*time.Second, 4, nil)
	}
	if cfg.RatePerSec == 0 {
		cfg.RatePerSec = 0.5 // public instance fair use
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 2
	}
	return &Source{cfg: cfg}
}

func (s *Source) Info() source.Info {
	return source.Info{
		Name:          Name,
		Description:   "OpenStreetMap via Overpass API — open data (ODbL), reusable, uneven coverage of contact fields.",
		Official:      true,
		DefaultCellKm: 15,
		MaxCells:      100,
		MinCellKm:     2,
		RatePerSec:    s.cfg.RatePerSec,
		Burst:         1,
		Concurrency:   s.cfg.Concurrency,
	}
}

var unsafeRe = regexp.MustCompile(`[^\p{L}\p{N} '\-&]`)

// BuildQuery renders the Overpass QL for a request.
func BuildQuery(req source.Request) string {
	bbox := fmt.Sprintf("(%.6f,%.6f,%.6f,%.6f)", req.Cell.MinLat, req.Cell.MinLng, req.Cell.MaxLat, req.Cell.MaxLng)
	var sb strings.Builder
	sb.WriteString("[out:json][timeout:60];\n(\n")
	if req.Sector != nil && len(req.Sector.OSMTags) > 0 && req.Query == "" {
		for _, t := range req.Sector.OSMTags {
			k, v, _ := strings.Cut(t, "=")
			fmt.Fprintf(&sb, "  nwr[%q=%q]%s;\n", k, v, bbox)
		}
	} else {
		q := strings.TrimSpace(unsafeRe.ReplaceAllString(req.SearchText(), " "))
		fmt.Fprintf(&sb, "  nwr[~\"^(shop|amenity|craft|office|healthcare|tourism|leisure)$\"~\".\"][\"name\"~%q,i]%s;\n", q, bbox)
		if req.Sector != nil {
			for _, t := range req.Sector.OSMTags {
				k, v, _ := strings.Cut(t, "=")
				fmt.Fprintf(&sb, "  nwr[%q=%q]%s;\n", k, v, bbox)
			}
		}
	}
	sb.WriteString(");\nout center tags qt;\n")
	return sb.String()
}

type element struct {
	Type   string                      `json:"type"`
	ID     int64                       `json:"id"`
	Lat    float64                     `json:"lat"`
	Lon    float64                     `json:"lon"`
	Center *struct{ Lat, Lon float64 } `json:"center"`
	Tags   map[string]string           `json:"tags"`
}

func (s *Source) Search(ctx context.Context, req source.Request) (source.Result, error) {
	if !req.Cell.Valid() {
		return source.Result{}, fmt.Errorf("%w: osm requires a geographic cell", source.ErrPermanent)
	}
	query := BuildQuery(req)
	var lastErr error
	// Try each endpoint once, starting from a rotating offset, and move on
	// only when the instance is overloaded.
	n := len(s.cfg.Endpoints)
	first := int(s.next.Add(1)) % n
	for i := 0; i < n; i++ {
		res, err := s.query(ctx, s.cfg.Endpoints[(first+i)%n], query, req.Country)
		if err == nil || !errors.Is(err, source.ErrRateLimited) || ctx.Err() != nil {
			return res, err
		}
		lastErr = err
	}
	return source.Result{}, lastErr
}

func (s *Source) query(ctx context.Context, endpoint, query, country string) (source.Result, error) {
	form := url.Values{"data": {query}}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return source.Result{}, err
	}
	hreq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	hreq.Header.Set("User-Agent", userAgent)
	resp, err := s.cfg.HTTPClient.Do(hreq)
	if err != nil {
		return source.Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		switch resp.StatusCode {
		case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			// Overpass signals "at capacity" with 429/502/503/504.
			return source.Result{}, &source.RetryAfterError{
				Err:   fmt.Errorf("%w: overpass %d at %s", source.ErrRateLimited, resp.StatusCode, endpoint),
				After: 60 * time.Second,
			}
		}
		return source.Result{}, source.ClassifyHTTP(resp, raw)
	}
	// Stream-decode: Overpass answers can be several MB.
	dec := json.NewDecoder(io.LimitReader(resp.Body, 64<<20))
	var payload struct {
		Remark   string    `json:"remark"`
		Elements []element `json:"elements"`
	}
	if err := dec.Decode(&payload); err != nil {
		return source.Result{}, fmt.Errorf("decode overpass: %w", err)
	}
	if strings.Contains(payload.Remark, "runtime error") {
		return source.Result{}, &source.RetryAfterError{Err: fmt.Errorf("%w: %s", source.ErrRateLimited, payload.Remark), After: 60 * time.Second}
	}
	out := source.Result{Businesses: make([]domain.Business, 0, len(payload.Elements))}
	for i := range payload.Elements {
		if b, ok := toBusiness(&payload.Elements[i], country); ok {
			out.Businesses = append(out.Businesses, b)
		}
	}
	return out, nil
}

func tag(t map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(t[k]); v != "" {
			return v
		}
	}
	return ""
}

func toBusiness(e *element, country string) (domain.Business, bool) {
	t := e.Tags
	name := tag(t, "name", "brand", "operator")
	if name == "" {
		return domain.Business{}, false
	}
	lat, lng := e.Lat, e.Lon
	if e.Center != nil {
		lat, lng = e.Center.Lat, e.Center.Lon
	}
	b := domain.Business{
		Name:       name,
		Street:     strings.TrimSpace(tag(t, "addr:housenumber") + " " + tag(t, "addr:street")),
		PostalCode: tag(t, "addr:postcode"),
		City:       tag(t, "addr:city"),
		Country:    country,
		Lat:        lat,
		Lng:        lng,
		Phone:      firstOf(tag(t, "phone", "contact:phone", "contact:mobile", "mobile")),
		Website:    tag(t, "website", "contact:website", "url"),
		OSMID:      e.Type + "/" + strconv.FormatInt(e.ID, 10),
		SourceURL:  "https://www.openstreetmap.org/" + e.Type + "/" + strconv.FormatInt(e.ID, 10),
		Sources:    []string{Name},
		Socials: domain.Socials{
			Facebook:  tag(t, "contact:facebook", "facebook"),
			Instagram: tag(t, "contact:instagram", "instagram"),
			LinkedIn:  tag(t, "contact:linkedin", "linkedin"),
			X:         tag(t, "contact:twitter", "twitter"),
		},
	}
	if e := firstOf(tag(t, "email", "contact:email")); e != "" {
		b.Emails = []string{e}
	}
	if siret := strings.ReplaceAll(tag(t, "ref:FR:SIRET"), " ", ""); len(siret) == 14 {
		b.SIRET, b.SIREN = siret, siret[:9]
	}
	if s := strings.ReplaceAll(tag(t, "ref:FR:SIREN"), " ", ""); b.SIREN == "" && len(s) == 9 {
		b.SIREN = s
	}
	for _, k := range []string{"craft", "shop", "amenity", "office", "healthcare", "tourism", "leisure"} {
		if v := t[k]; v != "" {
			b.Categories = append(b.Categories, k+"="+v)
		}
	}
	if len(b.Categories) > 0 {
		b.Category = b.Categories[0]
	}
	if b.Street != "" || b.City != "" {
		b.Address = strings.TrimSpace(strings.Trim(b.Street+", "+b.PostalCode+" "+b.City, ", "))
	}
	if oh := t["opening_hours"]; oh != "" {
		b.Hours = map[string]string{"osm": oh}
	}
	if t["disused:shop"] != "" || t["disused:amenity"] != "" {
		b.Status = domain.StatusClosedPermanently
	}
	return b, true
}

// firstOf keeps the first value of an OSM multi-value field ("a;b").
func firstOf(v string) string {
	if i := strings.IndexByte(v, ';'); i >= 0 {
		return strings.TrimSpace(v[:i])
	}
	return v
}
