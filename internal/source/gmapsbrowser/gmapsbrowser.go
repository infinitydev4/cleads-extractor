// Package gmapsbrowser delegates full-fidelity Google Maps extraction
// (place pages rendered by Chromium, optional email crawl) to a sidecar
// running the upstream gosom/google-maps-scraper image in web mode
// (`-web`). This is approach "A" applied where it is strongest: we reuse the
// battle-tested Playwright pipeline as a black box, isolated in its own
// Railway service so Chromium's RAM never lands in the API process.
//
// Compliance: same caveat as gmapshttp — opt-in, operator responsibility.
package gmapsbrowser

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/geo"
	"github.com/cleads/business-extractor/internal/source"
)

const Name = "google_maps_browser"

type Config struct {
	BaseURL      string // e.g. http://gmaps-scraper.railway.internal:8080
	HTTPClient   *http.Client
	Depth        int
	ExtractEmail bool
	MaxJobTime   time.Duration
	PollEvery    time.Duration
	Concurrency  int
}

type Source struct{ cfg Config }

func New(cfg Config) (*Source, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("%s: base URL is required", Name)
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = source.NewHTTPClient(60*time.Second, 16, nil)
	}
	if cfg.Depth == 0 {
		cfg.Depth = 5
	}
	if cfg.MaxJobTime == 0 {
		cfg.MaxJobTime = 10 * time.Minute
	}
	if cfg.PollEvery == 0 {
		cfg.PollEvery = 3 * time.Second
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 2
	}
	return &Source{cfg: cfg}, nil
}

func (s *Source) Info() source.Info {
	return source.Info{
		Name:          Name,
		Description:   "Full Google Maps extraction through a gosom/google-maps-scraper sidecar (Playwright). Slowest, richest; optional email crawl.",
		DefaultCellKm: 2,
		MaxCells:      200,
		MinCellKm:     0.5,
		RatePerSec:    0.5,
		Burst:         2,
		Concurrency:   s.cfg.Concurrency,
		Timeout:       s.cfg.MaxJobTime + 2*time.Minute,
	}
}

type upstreamJob struct {
	Name       string   `json:"name"`
	Keywords   []string `json:"keywords"`
	Lang       string   `json:"lang"`
	Zoom       int      `json:"zoom"`
	Lat        string   `json:"lat"`
	Lon        string   `json:"lon"`
	FastMode   bool     `json:"fast_mode"`
	Radius     int      `json:"radius"`
	Depth      int      `json:"depth"`
	Email      bool     `json:"email"`
	MaxTimeSec int64    `json:"max_time"`
}

func (s *Source) Search(ctx context.Context, req source.Request) (source.Result, error) {
	q := req.SearchText()
	if q == "" || !req.Cell.Valid() {
		return source.Result{}, fmt.Errorf("%w: query and cell are required", source.ErrPermanent)
	}
	c := req.Cell.Center()
	h, w := geo.SizeKm(req.Cell)
	job := upstreamJob{
		Name:       fmt.Sprintf("cleads-%s-%d", req.JobID, time.Now().UnixNano()),
		Keywords:   []string{q},
		Lang:       req.Language,
		Zoom:       geo.ZoomForCell(w),
		Lat:        strconv.FormatFloat(c.Lat, 'f', 6, 64),
		Lon:        strconv.FormatFloat(c.Lng, 'f', 6, 64),
		Radius:     int(max(h, w) * 1000 / 2 * 1.42),
		Depth:      s.cfg.Depth,
		Email:      s.cfg.ExtractEmail,
		MaxTimeSec: int64(s.cfg.MaxJobTime / time.Second),
	}
	id, err := s.create(ctx, job)
	if err != nil {
		return source.Result{}, err
	}
	defer s.delete(context.WithoutCancel(ctx), id)

	deadline := time.Now().Add(s.cfg.MaxJobTime + time.Minute)
	t := time.NewTicker(s.cfg.PollEvery)
	defer t.Stop()
	for {
		status, err := s.status(ctx, id)
		if err != nil {
			return source.Result{}, err
		}
		switch status {
		case "ok":
			items, err := s.download(ctx, id, req.Country)
			return source.Result{Businesses: items}, err
		case "failed":
			return source.Result{}, fmt.Errorf("upstream job %s failed", id)
		}
		if time.Now().After(deadline) {
			return source.Result{}, fmt.Errorf("upstream job %s timed out", id)
		}
		select {
		case <-ctx.Done():
			return source.Result{}, ctx.Err()
		case <-t.C:
		}
	}
}

func (s *Source) create(ctx context.Context, job upstreamJob) (string, error) {
	body, _ := json.Marshal(job)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.BaseURL+"/api/v1/jobs", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return "", source.ClassifyHTTP(resp, raw)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.ID == "" {
		return "", fmt.Errorf("upstream create: unexpected response %q", raw)
	}
	return out.ID, nil
}

func (s *Source) status(ctx context.Context, id string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.BaseURL+"/api/v1/jobs/"+id, nil)
	if err != nil {
		return "", err
	}
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", source.ClassifyHTTP(resp, raw)
	}
	var out struct {
		Status string `json:"Status"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	return strings.ToLower(out.Status), nil
}

func (s *Source) delete(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.cfg.BaseURL+"/api/v1/jobs/"+id, nil)
	if err != nil {
		return
	}
	if resp, err := s.cfg.HTTPClient.Do(req); err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func (s *Source) download(ctx context.Context, id, country string) ([]domain.Business, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.BaseURL+"/api/v1/jobs/"+id+"/download", nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, source.ClassifyHTTP(resp, raw)
	}
	return ParseCSV(resp.Body, country)
}

// ParseCSV converts the upstream CSV export, streaming row by row.
func ParseCSV(r io.Reader, country string) ([]domain.Business, error) {
	cr := csv.NewReader(r)
	cr.ReuseRecord = true
	cr.FieldsPerRecord = -1
	header, err := cr.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, err
	}
	col := make(map[string]int, len(header))
	for i, h := range header {
		col[strings.TrimSpace(h)] = i
	}
	get := func(rec []string, name string) string {
		if i, ok := col[name]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	var out []domain.Business
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return out, err
		}
		b := domain.Business{
			Name:      get(rec, "title"),
			Category:  get(rec, "category"),
			Address:   get(rec, "address"),
			Website:   get(rec, "website"),
			Phone:     get(rec, "phone"),
			CID:       get(rec, "cid"),
			PlaceID:   get(rec, "place_id"),
			SourceURL: get(rec, "link"),
			Country:   country,
			Sources:   []string{Name},
		}
		if b.Name == "" {
			continue
		}
		b.Rating, _ = strconv.ParseFloat(get(rec, "review_rating"), 64)
		b.ReviewsCount, _ = strconv.Atoi(get(rec, "review_count"))
		b.Lat, _ = strconv.ParseFloat(get(rec, "latitude"), 64)
		b.Lng, _ = strconv.ParseFloat(get(rec, "longitude"), 64)
		b.Status = statusOf(get(rec, "status"))
		if b.Category != "" {
			b.Categories = []string{b.Category}
		}
		var addr struct {
			Street     string `json:"street"`
			City       string `json:"city"`
			PostalCode string `json:"postal_code"`
			State      string `json:"state"`
		}
		if ca := get(rec, "complete_address"); strings.HasPrefix(ca, "{") && json.Unmarshal([]byte(ca), &addr) == nil {
			b.Street, b.City, b.PostalCode, b.Region = addr.Street, addr.City, addr.PostalCode, addr.State
		}
		if em := get(rec, "emails"); em != "" {
			for _, e := range strings.FieldsFunc(em, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
				b.Emails = append(b.Emails, e)
			}
		}
		var hours map[string][]string
		if oh := get(rec, "open_hours"); strings.HasPrefix(oh, "{") && json.Unmarshal([]byte(oh), &hours) == nil && len(hours) > 0 {
			b.Hours = make(map[string]string, len(hours))
			for d, slots := range hours {
				b.Hours[d] = strings.Join(slots, ", ")
			}
		}
		out = append(out, b)
	}
	return out, nil
}

func statusOf(s string) string {
	l := strings.ToLower(s)
	switch {
	case l == "":
		return ""
	case strings.Contains(l, "permanent"):
		return domain.StatusClosedPermanently
	case strings.Contains(l, "temporar"):
		return domain.StatusClosedTemporarily
	}
	return domain.StatusOperational
}
