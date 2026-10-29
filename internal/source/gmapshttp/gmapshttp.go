// Package gmapshttp queries Google Maps' internal search endpoint over plain
// HTTP ("fast mode" in gosom/google-maps-scraper). No browser is involved,
// which makes it ~2 orders of magnitude cheaper in RAM/CPU than a Playwright
// pipeline, at the cost of fewer fields (no emails, no reviews) and of a
// format that can change without notice.
//
// It is the default Google source (no API key needed). Automated access to
// Google Maps is not covered by Google's terms: the operator is responsible
// for its use (see docs/COMPLIANCE.md). Disable with ENABLE_GMAPS_SCRAPING=false.
package gmapshttp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/geo"
	"github.com/cleads/business-extractor/internal/source"
)

const (
	Name          = "google_maps_http"
	defaultURL    = "https://www.google.com/search"
	detailsURL    = "https://www.google.com/maps/preview/place"
	perPage       = 20
	defaultCookie = "SOCS=CAESEwgDEgk0ODE3Nzk3MjQaAmVuIAEaBgiA_LyaBg; CONSENT=PENDING+987"
	userAgent     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"
)

var errInvalidPayload = errors.New("gmaps: unexpected payload format")

type Config struct {
	Endpoint        string
	DetailsEndpoint string
	// DebugDir, when set, receives every payload the parser rejects.
	DebugDir    string
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
	if cfg.DetailsEndpoint == "" {
		cfg.DetailsEndpoint = detailsURL
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
		cfg.MaxPages = 6 // 120 results, the depth of the Maps result list
	}
	// Measured from one residential IP (docs/BENCHMARKS.md): 0 errors up to
	// ~20 req/s over 5-minute runs, slowdowns after ~25k requests at 20 req/s.
	// 8 req/s per IP leaves headroom; scale out with proxies or workers.
	if cfg.RatePerSec == 0 {
		cfg.RatePerSec = 8
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 24
	}
	return &Source{cfg: cfg}
}

func (s *Source) Info() source.Info {
	return source.Info{
		Name:          Name,
		Description:   "Google Maps over plain HTTP (no browser, no API key): search pages then full place profiles (description, attributes, photos, owner, reviews, popular times).",
		DefaultCellKm: 0.75, // measured: +18% businesses vs 1.5 km on Lyon
		MaxCells:      1500,
		MinCellKm:     0.2,
		RatePerSec:    s.cfg.RatePerSec,
		Burst:         int(s.cfg.RatePerSec*2) + 1,
		Concurrency:   s.cfg.Concurrency,
		LooseResults:  true,
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
		s.dump("search", body)
		return source.Result{}, err
	}
	// The endpoint ranks by distance to the viewport center but does not
	// restrict to it: keep everything (dedup merges it) and decide on
	// pagination from the share of results that still fall in the cell.
	pad := padded(req.Cell, 0.5)
	inCell := 0
	kept := items[:0]
	for i := range items {
		if items[i].Lat == 0 && items[i].Lng == 0 {
			continue
		}
		if pad.Contains(domain.Point{Lat: items[i].Lat, Lng: items[i].Lng}) {
			inCell++
		}
		items[i].Country = req.Country
		kept = append(kept, items[i])
	}
	res := source.Result{Businesses: kept}
	if len(items) >= perPage && inCell*2 >= len(items) {
		if req.Page+1 < s.cfg.MaxPages {
			res.NextPageToken = fmt.Sprintf("offset:%d", offset+perPage)
		} else {
			res.Saturated = true
		}
	}
	return res, nil
}

func padded(b domain.BBox, ratio float64) domain.BBox {
	dLat := (b.MaxLat - b.MinLat) * ratio / 2
	dLng := (b.MaxLng - b.MinLng) * ratio / 2
	return domain.BBox{MinLat: b.MinLat - dLat, MinLng: b.MinLng - dLng, MaxLat: b.MaxLat + dLat, MaxLng: b.MaxLng + dLng}
}

// Details fetches the full place profile. It calls the JSON endpoint the
// Maps web app uses after opening a place (/maps/preview/place), keyed only
// by the feature id: one ~40 KB request, no HTML page, no browser.
func (s *Source) Details(ctx context.Context, req source.DetailsRequest) (domain.Business, error) {
	if req.DataID == "" {
		return domain.Business{}, fmt.Errorf("%w: missing data id", source.ErrPermanent)
	}
	q := url.Values{}
	q.Set("authuser", "0")
	q.Set("hl", req.Language)
	q.Set("gl", strings.ToLower(req.Country))
	q.Set("pb", buildDetailsPB(req.DataID, req.Name))
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.DetailsEndpoint+"?"+q.Encode(), nil)
	if err != nil {
		return domain.Business{}, err
	}
	hreq.Header.Set("User-Agent", userAgent)
	hreq.Header.Set("Accept", "*/*")
	hreq.Header.Set("Accept-Language", req.Language)
	hreq.Header.Set("Cookie", s.cfg.Cookie)
	resp, err := s.cfg.HTTPClient.Do(hreq)
	if err != nil {
		return domain.Business{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return domain.Business{}, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return domain.Business{}, fmt.Errorf("%w: redirected to %s", source.ErrBlocked, resp.Header.Get("Location"))
	}
	if resp.StatusCode != http.StatusOK {
		return domain.Business{}, source.ClassifyHTTP(resp, body)
	}
	if isCaptcha(body) {
		return domain.Business{}, fmt.Errorf("%w: captcha page", source.ErrBlocked)
	}
	b, err := ParsePlace(body)
	if err != nil {
		s.dump("place", body)
		return b, err
	}
	b.Country = req.Country
	return b, nil
}

// buildDetailsPB is the place-request protobuf captured from the Maps web
// app (October 2026), parameterized by feature id. The viewport is ignored by
// the endpoint; the session token (!14m3) is not validated but its presence
// is what makes the response include reviews, so a random one is sent.
func buildDetailsPB(dataID, name string) string {
	if name == "" {
		name = "x"
	}
	name = strings.NewReplacer("!", " ", "*", " ").Replace(name)
	return "!1m15!1s" + dataID + "!2s" + name +
		"!3m12!1m3!1d10000!2d2.35!3d48.85!2m3!1f0.0!2f0.0!3f0.0!3m2!1i1024!2i768!4f13.1" +
		"!12m4!2m3!1i360!2i120!4i8!13m57!2m2!1i203!2i100!3m2!2i4!5b1!6m6!1m2!1i86!2i86!1m2!1i408!2i240" +
		"!7m33!1m3!1e1!2b0!3e3!1m3!1e2!2b1!3e2!1m3!1e2!2b0!3e3!1m3!1e8!2b0!3e3!1m3!1e10!2b0!3e3!1m3!1e10!2b1!3e2" +
		"!1m3!1e10!2b0!3e4!1m3!1e9!2b1!3e2!2b1!9b0!15m8!1m7!1m2!1m1!1e2!2m2!1i195!2i195!3i20" +
		"!14m3!1s" + sessionToken() + "!7e81!15i10112" +
		"!15m108!1m26!13m9!2b1!3b1!4b1!6i1!8b1!9b1!14b1!20b1!25b1!18m15!3b1!4b1!5b1!6b1!13b1!14b1!17b1!21b1!22b1" +
		"!30b1!32b1!33m1!1b1!34b1!36e2!10m1!8e3!11m1!3e1!17b1!20m2!1e3!1e6!24b1!25b1!26b1!27b1!29b1!30m1!2b1!36b1" +
		"!37b1!39m3!2m2!2i1!3i1!43b1!52b1!54m1!1b1!55b1!56m1!1b1!61m2!1m1!1e1!65m5!3m4!1m3!1m2!1i224!2i298" +
		"!72m22!1m8!2b1!5b1!7b1!12m4!1b1!2b1!4m1!1e1!4b1!8m10!1m6!4m1!1e1!4m1!1e3!4m1!1e4" +
		"!3sother_user_google_review_posts__and__hotel_and_vr_partner_review_posts!6m1!1e1!9b1!89b1!90m2!1m1!1e2" +
		"!98m3!1b1!2b1!3b1!103b1!113b1!114m3!1b1!2m1!1b1!117b1!122m1!1b1!126b1!127b1!128m1!1b1!21m0!22m1!1e81" +
		"!30m8!3b1!6m2!1b1!2b1!7m2!1e3!2b1!9b1!34m5!7b1!10b1!14b1!15m1!1b0!37i801"
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

// sessionToken mimics the 22-char base64url "ei" token of a Maps session.
func sessionToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])[:22]
}

func (s *Source) dump(kind string, body []byte) {
	if s.cfg.DebugDir == "" {
		return
	}
	name := filepath.Join(s.cfg.DebugDir, fmt.Sprintf("%s-%d.json", kind, time.Now().UnixNano()))
	_ = os.WriteFile(name, body, 0o600)
}
