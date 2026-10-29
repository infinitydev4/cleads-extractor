// Package gmapshttp queries Google Maps' internal search endpoint over plain
// HTTP ("fast mode" in gosom/google-maps-scraper). No browser is involved,
// which makes it ~2 orders of magnitude cheaper in RAM/CPU than a Playwright
// pipeline, at the cost of fewer fields (no emails, no reviews) and of a
// format that can change without notice.
//
// Compliance: automated access to Google Maps is not permitted by Google's
// Terms of Service. This source is disabled unless ENABLE_GMAPS_SCRAPING=true
// and the operator is responsible for its use. Prefer google_places.
package gmapshttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cleads/business-extractor/internal/geo"
	"github.com/cleads/business-extractor/internal/source"
)

const (
	Name          = "google_maps_http"
	defaultURL    = "https://www.google.com/search"
	perPage       = 20
	defaultCookie = "SOCS=CAESEwgDEgk0ODE3Nzk3MjQaAmVuIAEaBgiA_LyaBg; CONSENT=PENDING+987"
	userAgent     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"
)

var errInvalidPayload = errors.New("gmaps: unexpected payload format")

type Config struct {
	Endpoint    string
	HTTPClient  *http.Client
	Cookie      string
	MaxPages    int // offset pagination depth per cell (20 results per page)
	RatePerSec  float64
	Concurrency int
}

type Source struct{ cfg Config }

func New(cfg Config) *Source {
	if cfg.Endpoint == "" {
		cfg.Endpoint = defaultURL
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = source.NewHTTPClient(15*time.Second, 32, nil)
	}
	// A redirect to /sorry/ or consent.google.com means we are blocked; do
	// not follow it so it can be classified.
	c := *cfg.HTTPClient
	c.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if strings.Contains(req.URL.Path, "/sorry/") || strings.HasPrefix(req.URL.Host, "consent.") {
			return http.ErrUseLastResponse
		}
		return nil
	}
	cfg.HTTPClient = &c
	if cfg.Cookie == "" {
		cfg.Cookie = defaultCookie
	}
	if cfg.MaxPages <= 0 {
		cfg.MaxPages = 3
	}
	if cfg.RatePerSec == 0 {
		cfg.RatePerSec = 2
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 8
	}
	return &Source{cfg: cfg}
}

func (s *Source) Info() source.Info {
	return source.Info{
		Name:          Name,
		Description:   "Google Maps internal search over HTTP (no browser). Unofficial; opt-in; fields: name, category, address, phone, website, rating, hours, GPS.",
		DefaultCellKm: 1.5,
		MaxCells:      900,
		MinCellKm:     0.25,
		RatePerSec:    s.cfg.RatePerSec,
		Burst:         int(s.cfg.RatePerSec*2) + 1,
		Concurrency:   s.cfg.Concurrency,
	}
}

func (s *Source) Search(ctx context.Context, req source.Request) (source.Result, error) {
	q := req.SearchText()
	if q == "" {
		return source.Result{}, fmt.Errorf("%w: empty query", source.ErrPermanent)
	}
	if !req.Cell.Valid() {
		return source.Result{}, fmt.Errorf("%w: %s requires a geographic cell", source.ErrPermanent, Name)
	}
	_, wKm := geo.SizeKm(req.Cell)
	c := req.Cell.Center()
	offset := req.Page * perPage

	params := url.Values{}
	params.Set("tbm", "map")
	params.Set("authuser", "0")
	params.Set("hl", req.Language)
	params.Set("gl", strings.ToLower(req.Country))
	params.Set("q", q)
	params.Set("pb", buildPB(c.Lat, c.Lng, geo.ZoomForCell(wKm), offset))

	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.Endpoint+"?"+params.Encode(), nil)
	if err != nil {
		return source.Result{}, err
	}
	hreq.Header.Set("User-Agent", userAgent)
	hreq.Header.Set("Accept", "*/*")
	hreq.Header.Set("Accept-Language", req.Language)
	hreq.Header.Set("Cookie", s.cfg.Cookie)

	resp, err := s.cfg.HTTPClient.Do(hreq)
	if err != nil {
		return source.Result{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return source.Result{}, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return source.Result{}, fmt.Errorf("%w: redirected to %s", source.ErrBlocked, resp.Header.Get("Location"))
	}
	if resp.StatusCode != http.StatusOK {
		return source.Result{}, source.ClassifyHTTP(resp, body)
	}
	if isCaptcha(body) {
		return source.Result{}, fmt.Errorf("%w: captcha page", source.ErrBlocked)
	}
	items, err := ParseSearch(body)
	if err != nil {
		return source.Result{}, err
	}
	// The endpoint biases by viewport but does not restrict to it.
	kept := items[:0]
	for i := range items {
		if items[i].Lat == 0 && items[i].Lng == 0 {
			continue
		}
		items[i].Country = req.Country
		kept = append(kept, items[i])
	}
	res := source.Result{Businesses: kept}
	if len(items) >= perPage {
		if req.Page+1 < s.cfg.MaxPages {
			res.NextPageToken = fmt.Sprintf("offset:%d", offset+perPage)
		} else {
			res.Saturated = true
		}
	}
	return res, nil
}

func isCaptcha(body []byte) bool {
	if len(body) > 0 && body[0] == '<' {
		l := strings.ToLower(string(body[:min(len(body), 4096)]))
		return strings.Contains(l, "unusual traffic") || strings.Contains(l, "captcha") || strings.Contains(l, "/sorry/")
	}
	return false
}

// buildPB builds the protobuf-ish "pb" parameter of the map search. Layout
// from gosom/google-maps-scraper (MIT); !7i is the page size and !8i the
// offset.
func buildPB(lat, lng float64, zoom, offset int) string {
	return fmt.Sprintf("!4m12!1m3!1d3826.902183192154!2d%.6f!3d%.6f!2m3!1f0!2f0!3f0!3m2!1i600!2i800!4f%d.0!7i%d!8i%d"+
		"!10b1!12m22!1m3!18b1!30b1!34e1!2m3!5m1!6e2!20e3!4b0!10b1!12b1!13b1!16b1!17m1!3e1!20m3!5e2!6b1!14b1!46m1!1b0"+
		"!96b1!19m4!2m3!1i360!2i120!4i8",
		lng, lat, zoom, perPage, offset)
}
