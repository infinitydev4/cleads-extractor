package api

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cleads/business-extractor/internal/config"
	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/engine"
	"github.com/cleads/business-extractor/internal/geocode"
	"github.com/cleads/business-extractor/internal/metrics"
	"github.com/cleads/business-extractor/internal/source"
	"github.com/cleads/business-extractor/internal/source/mock"
	"github.com/cleads/business-extractor/internal/store/memstore"
)

const (
	keyA = "key-a-0123456789abcdefghijkl"
	keyB = "key-b-0123456789abcdefghijkl"
)

func setup(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	cfg := &config.Config{
		APIKeys:       []config.APIKey{{ID: "crm", Hash: config.KeyHash(keyA)}, {ID: "other", Hash: config.KeyHash(keyB)}},
		RatePerKey:    1000,
		MaxResultsCap: 10_000,
		MaxBodyBytes:  1 << 16,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := memstore.New()
	m := metrics.New()
	eng := engine.New(engine.Config{PollMin: 5 * time.Millisecond, PollMax: 20 * time.Millisecond, FinalizeEvery: 20 * time.Millisecond},
		st, []source.Source{mock.New(mock.Config{CellKm: 1})},
		geocode.Static{"lyon": {MinLat: 45.74, MinLng: 4.80, MaxLat: 45.76, MaxLng: 4.83}}, nil, m, log)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = eng.Run(ctx); close(done) }()
	srv := httptest.NewServer(New(cfg, st, eng, m, log, "test").Handler())
	return srv, func() { srv.Close(); cancel(); <-done }
}

func do(t *testing.T, method, url, key, body string, hdr ...string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestAuth(t *testing.T) {
	srv, stop := setup(t)
	defer stop()
	if r, _ := do(t, "GET", srv.URL+"/v1/sources", "", ""); r.StatusCode != 401 {
		t.Fatalf("no key: %d", r.StatusCode)
	}
	if r, _ := do(t, "GET", srv.URL+"/v1/sources", "wrong-key-wrong-key-wrong", ""); r.StatusCode != 401 {
		t.Fatalf("bad key: %d", r.StatusCode)
	}
	if r, _ := do(t, "GET", srv.URL+"/v1/sources", "", "", "X-API-Key", keyA); r.StatusCode != 200 {
		t.Fatalf("X-API-Key: %d", r.StatusCode)
	}
	if r, _ := do(t, "GET", srv.URL+"/healthz", "", ""); r.StatusCode != 200 {
		t.Fatalf("healthz must be public: %d", r.StatusCode)
	}
	if r, _ := do(t, "GET", srv.URL+"/metrics", "", ""); r.StatusCode != 401 {
		t.Fatalf("metrics must require auth: %d", r.StatusCode)
	}
}

func TestValidation(t *testing.T) {
	srv, stop := setup(t)
	defer stop()
	cases := map[string]int{
		`{`:                       400,
		`{"query":"x","bogus":1}`: 400,
		`{"query":"x"}`:           422,
		`{"query":"x","location":"Lyon","sources":["nope"]}`:                      422,
		`{"sector":"nope","location":"Lyon"}`:                                     422,
		`{"query":"x","location":"Lyon","max_results":999999}`:                    422,
		`{"query":"x","location":"Lyon","center":{"lat":1,"lng":1},"radius_m":5}`: 422,
	}
	for body, want := range cases {
		if r, b := do(t, "POST", srv.URL+"/v1/jobs", keyA, body); r.StatusCode != want {
			t.Errorf("%s -> %d (want %d): %s", body, r.StatusCode, want, b)
		}
	}
}

func TestJobLifecycle(t *testing.T) {
	srv, stop := setup(t)
	defer stop()

	r, b := do(t, "POST", srv.URL+"/v1/jobs", keyA, `{"query":"plombier","location":"Lyon"}`, "Idempotency-Key", "abc")
	if r.StatusCode != 202 {
		t.Fatalf("create: %d %s", r.StatusCode, b)
	}
	var created struct {
		Job            domain.Job `json:"job"`
		PlannedSources []string   `json:"planned_sources"`
	}
	_ = json.Unmarshal(b, &created)
	id := created.Job.ID
	if id == "" || r.Header.Get("Location") != "/v1/jobs/"+id || len(created.PlannedSources) != 1 {
		t.Fatalf("bad create response %s", b)
	}
	// idempotent replay
	r, b = do(t, "POST", srv.URL+"/v1/jobs", keyA, `{"query":"plombier","location":"Lyon"}`, "Idempotency-Key", "abc")
	if r.StatusCode != 200 || !strings.Contains(string(b), id) {
		t.Fatalf("replay: %d %s", r.StatusCode, b)
	}
	// tenant isolation
	if r, _ := do(t, "GET", srv.URL+"/v1/jobs/"+id, keyB, ""); r.StatusCode != 404 {
		t.Fatalf("other tenant sees the job: %d", r.StatusCode)
	}

	// SSE until done
	req, _ := http.NewRequest("GET", srv.URL+"/v1/jobs/"+id+"/events?interval_ms=250", nil)
	req.Header.Set("Authorization", "Bearer "+keyA)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %s", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	progress, done := 0, false
	deadline := time.Now().Add(15 * time.Second)
	for sc.Scan() && time.Now().Before(deadline) {
		line := sc.Text()
		if line == "event: progress" {
			progress++
		}
		if line == "event: done" {
			done = true
			break
		}
	}
	resp.Body.Close()
	if !done || progress == 0 {
		t.Fatalf("sse: progress=%d done=%v", progress, done)
	}

	// progressive listing with cursor
	var total int
	cursor := "0"
	for i := 0; i < 100; i++ {
		_, b = do(t, "GET", srv.URL+"/v1/jobs/"+id+"/businesses?limit=7&cursor="+cursor, keyA, "")
		var page struct {
			Items      []json.RawMessage `json:"items"`
			NextCursor string            `json:"next_cursor"`
			HasMore    bool              `json:"has_more"`
			Counters   domain.Counters   `json:"counters"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			t.Fatal(err, string(b))
		}
		total += len(page.Items)
		cursor = page.NextCursor
		if !page.HasMore {
			if int64(total) != page.Counters.Unique {
				t.Fatalf("paged %d items, unique counter %d", total, page.Counters.Unique)
			}
			break
		}
	}
	if total == 0 {
		t.Fatal("no results")
	}

	// CSV export
	r, b = do(t, "GET", srv.URL+"/v1/jobs/"+id+"/export?format=csv&has_phone=true", keyA, "")
	if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/csv") {
		t.Fatalf("export: %d %s", r.StatusCode, r.Header.Get("Content-Type"))
	}
	rows, err := csv.NewReader(strings.NewReader(string(b))).ReadAll()
	if err != nil || len(rows) < 2 {
		t.Fatalf("csv: %v rows=%d", err, len(rows))
	}
	for _, row := range rows[1:] {
		if row[3] == "" {
			t.Fatal("has_phone filter not applied")
		}
	}

	// consolidated base
	r, b = do(t, "GET", srv.URL+"/v1/businesses?limit=5&has_phone=true", keyA, "")
	if r.StatusCode != 200 || !strings.Contains(string(b), `"items"`) {
		t.Fatalf("businesses: %d %s", r.StatusCode, b)
	}
	// cancel finished job -> 409
	if r, _ := do(t, "DELETE", srv.URL+"/v1/jobs/"+id, keyA, ""); r.StatusCode != 409 {
		t.Fatalf("cancel finished: %d", r.StatusCode)
	}
}

func TestRateLimitPerKey(t *testing.T) {
	cfg := &config.Config{APIKeys: []config.APIKey{{ID: "k", Hash: config.KeyHash(keyA)}}, RatePerKey: 1, MaxBodyBytes: 1024}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := memstore.New()
	m := metrics.New()
	eng := engine.New(engine.Config{}, st, []source.Source{mock.New(mock.Config{})}, nil, nil, m, log)
	srv := httptest.NewServer(New(cfg, st, eng, m, log, "test").Handler())
	defer srv.Close()
	limited := false
	for i := 0; i < 10; i++ {
		if r, _ := do(t, "GET", srv.URL+"/v1/sectors", keyA, ""); r.StatusCode == 429 {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("per-key rate limit never triggered")
	}
}
