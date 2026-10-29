package obs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/cleads/business-extractor/internal/domain"
)

func TestRedactor(t *testing.T) {
	r := NewRedactor("live-key-0123456789abcdef", "s3cr3t-webhook")
	cases := map[string][]string{ // input -> secrets that must disappear
		"proxyconnect tcp: dial http://alice:hunter2@proxy.example:8080 failed": {"alice", "hunter2"},
		"socks5://bob:pa55@10.0.0.1:1080":                                       {"bob", "pa55"},
		"connect postgres://cleads:dbpass@db.railway.internal:5432/railway":     {"dbpass"},
		"host=db user=cleads password=dbpass2 sslmode=require":                  {"dbpass2"},
		"Authorization: Bearer abcdefghijklmnop0123":                            {"abcdefghijklmnop0123"},
		`{"x-api-key":"k-0123456789abcdefghijk"}`:                               {"k-0123456789abcdefghijk"},
		"GET https://maps.googleapis.com/x?key=AIzaSyD-secret&q=plombier: 403":  {"AIzaSyD-secret"},
		"unauthorized with live-key-0123456789abcdef":                           {"live-key-0123456789abcdef"},
		"webhook signed with s3cr3t-webhook":                                    {"s3cr3t-webhook"},
		"api_key=zz-very-secret-value, retrying":                                {"zz-very-secret-value"},
	}
	for in, secrets := range cases {
		out := r.String(in)
		for _, s := range secrets {
			if strings.Contains(out, s) {
				t.Errorf("%q -> %q still contains %q", in, out, s)
			}
		}
	}
	// Ordinary text is left alone.
	for _, s := range []string{"http 500 on https://www.google.com/maps/preview/place", "design: ok", "tokens: 5", "plombier Lyon"} {
		if got := r.String(s); got != s {
			t.Errorf("%q altered to %q", s, got)
		}
	}
	m := r.Value(map[string]any{"password": "x", "nested": []any{"http://u:p@h"}}).(map[string]any)
	if m["password"] != masked || strings.Contains(m["nested"].([]any)[0].(string), "u:p") {
		t.Fatalf("Value did not redact: %v", m)
	}
}

func TestLoggerRedactsStdoutAndEvents(t *testing.T) {
	var out bytes.Buffer
	log, tee := NewLogger(&out, "json", slog.LevelInfo, TeeOptions{Redactor: NewRedactor("topsecret-api-key-123"), RingSize: 10})
	log.Warn("proxy failed", "proxy", "http://user:pw123@p:1", "err", errors.New("auth topsecret-api-key-123 refused"),
		"authorization", "Bearer xyz")
	if s := out.String(); strings.Contains(s, "pw123") || strings.Contains(s, "topsecret") || strings.Contains(s, "xyz") {
		t.Fatalf("stdout leaks a secret: %s", s)
	}
	ev := <-tee.core.ch
	b := strings.Join([]string{ev.Msg, ev.Attrs["proxy"].(string), ev.Attrs["err"].(string), ev.Attrs["authorization"].(string)}, " ")
	if strings.Contains(b, "pw123") || strings.Contains(b, "topsecret") || strings.Contains(b, "xyz") {
		t.Fatalf("event leaks a secret: %s", b)
	}
	for _, e := range tee.Ring().Snapshot(slog.LevelInfo, "", "", 10) {
		if strings.Contains(e.Attrs["proxy"].(string), "pw123") {
			t.Fatal("ring leaks a secret")
		}
	}
}

type memSink struct {
	mu     sync.Mutex
	events []domain.LogEvent
	block  chan struct{} // when non-nil, inserts wait for it
	purged atomic.Int64
}

func (s *memSink) InsertEvents(ctx context.Context, evs []domain.LogEvent) error {
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	s.events = append(s.events, evs...)
	s.mu.Unlock()
	return nil
}

func (s *memSink) PurgeEvents(context.Context, time.Time) (int64, error) {
	s.purged.Add(1)
	return 0, nil
}

func (s *memSink) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

func TestTeeExtractsColumnsAndBatches(t *testing.T) {
	log, tee := NewLogger(io.Discard, "json", slog.LevelInfo, TeeOptions{Worker: "w1", FlushEvery: 10 * time.Millisecond, Retention: time.Hour})
	sink := &memSink{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tee.Run(ctx, sink); close(done) }()

	log.Info("not stored")
	log.With("source", "google_maps_http.details").Warn("source error (will retry)", "job", "job_1", "task", int64(42), "err", errors.New("http 500"))
	log.WithGroup("g").Error("boom", "k", 1)
	deadline := time.Now().Add(2 * time.Second)
	for sink.len() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if sink.len() != 2 {
		t.Fatalf("stored %d events, want 2", sink.len())
	}
	e := sink.events[0]
	if e.JobID != "job_1" || e.TaskID != 42 || e.Source != "google_maps_http.details" || e.Worker != "w1" || e.Level != "WARN" || e.Attrs["err"] != "http 500" {
		t.Fatalf("bad event %+v", e)
	}
	if sink.events[1].Attrs["g.k"] != int64(1) {
		t.Fatalf("group attr: %+v", sink.events[1].Attrs)
	}
	if sink.purged.Load() == 0 {
		t.Fatal("retention set but no purge ran")
	}
}

// The engine must never wait for the database: with a sink that blocks
// forever and a tiny queue, logging stays fast and losses are counted.
func TestTeeNeverBlocksUnderLoad(t *testing.T) {
	events := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "x"}, []string{"outcome"})
	log, tee := NewLogger(io.Discard, "json", slog.LevelInfo, TeeOptions{Buffer: 16, Batch: 4, Events: events})
	sink := &memSink{block: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tee.Run(ctx, sink); close(done) }()

	const goroutines, perG = 8, 5000
	start := time.Now()
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				log.Warn("source error (will retry)", "job", "j", "task", i, "source", "mock")
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("logging %d warnings took %s: the tee blocks", goroutines*perG, elapsed)
	}
	var m dto.Metric
	_ = events.WithLabelValues("dropped").Write(&m)
	dropped := m.GetCounter().GetValue()
	if dropped < goroutines*perG-100 {
		t.Fatalf("dropped %.0f, expected nearly all of %d", dropped, goroutines*perG)
	}
	close(sink.block)
	cancel()
	<-done
	t.Logf("%d warnings in %s (%.0f ns/op), %.0f dropped", goroutines*perG, elapsed,
		float64(elapsed.Nanoseconds())/float64(goroutines*perG), dropped)
}

func TestRingSnapshot(t *testing.T) {
	log, tee := NewLogger(io.Discard, "json", slog.LevelInfo, TeeOptions{RingSize: 3})
	for i := 0; i < 5; i++ {
		log.Info("tick", "job", "j", "i", i)
	}
	log.Debug("hidden")
	got := tee.Ring().Snapshot(slog.LevelInfo, "j", "", 10)
	if len(got) != 3 || got[0].Attrs["i"] != int64(4) || got[2].Attrs["i"] != int64(2) {
		t.Fatalf("ring: %+v", got)
	}
	if n := len(tee.Ring().Snapshot(slog.LevelWarn, "", "", 10)); n != 0 {
		t.Fatalf("level filter: %d", n)
	}
}

func BenchmarkTeeWarn(b *testing.B) {
	log, tee := NewLogger(io.Discard, "json", slog.LevelInfo, TeeOptions{RingSize: 1000})
	sink := &memSink{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tee.Run(ctx, sink)
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		log.Warn("source error (will retry)", "source", "mock", "job", "job_x", "task", int64(i), "err", errors.New("http 500"))
	}
}

func BenchmarkTeeInfo(b *testing.B) {
	log, _ := NewLogger(io.Discard, "json", slog.LevelInfo, TeeOptions{RingSize: 1000})
	b.ReportAllocs()
	for b.Loop() {
		log.Info("http", "method", "GET", "route", "GET /v1/jobs/{id}", "status", 200, "dur_ms", 0.4, "request_id", "abcd", "key", "crm")
	}
}
