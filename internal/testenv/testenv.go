// Package testenv starts a complete in-process extractor for tests: mock
// source, memstore, teed logs and the HTTP API with three API keys. It never
// calls the network.
package testenv

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/cleads/business-extractor/internal/api"
	"github.com/cleads/business-extractor/internal/config"
	"github.com/cleads/business-extractor/internal/engine"
	"github.com/cleads/business-extractor/internal/geocode"
	"github.com/cleads/business-extractor/internal/metrics"
	"github.com/cleads/business-extractor/internal/obs"
	"github.com/cleads/business-extractor/internal/source"
	"github.com/cleads/business-extractor/internal/source/mock"
	"github.com/cleads/business-extractor/internal/store/memstore"
)

// API keys of the test server (ids crm, other and admin).
const (
	KeyCRM   = "key-crm-0123456789abcdefghij"
	KeyOther = "key-other-0123456789abcdefgh"
	KeyAdmin = "key-admin-0123456789abcdefgh"
)

// Start runs the extractor and its HTTP API until the test ends. The
// geocoder knows the location "Lyon".
func Start(t testing.TB) *httptest.Server {
	t.Helper()
	cfg := &config.Config{
		APIKeys: []config.APIKey{{ID: "crm", Hash: config.KeyHash(KeyCRM)}, {ID: "other", Hash: config.KeyHash(KeyOther)},
			{ID: "admin", Hash: config.KeyHash(KeyAdmin)}},
		RatePerKey: 1000, MaxResultsCap: 10_000, MaxBodyBytes: 1 << 20,
		MCPEnabled: true, MCPMaxWait: 20 * time.Second, Secrets: []string{KeyCRM, KeyOther, KeyAdmin},
	}
	log, tee := obs.NewLogger(io.Discard, "json", slog.LevelInfo, obs.TeeOptions{Worker: "test", RingSize: 100,
		FlushEvery: 10 * time.Millisecond, Redactor: obs.NewRedactor(cfg.Secrets...)})
	st := memstore.New()
	m := metrics.New()
	eng := engine.New(engine.Config{PollMin: 5 * time.Millisecond, PollMax: 20 * time.Millisecond, FinalizeEvery: 20 * time.Millisecond,
		WorkerID: "w1", StatusEvery: 20 * time.Millisecond},
		st, []source.Source{mock.New(mock.Config{CellKm: 1, Latency: 5 * time.Millisecond})},
		geocode.Static{"lyon": {MinLat: 45.74, MinLng: 4.80, MaxLat: 45.76, MaxLng: 4.83}}, nil, m, log)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = eng.Run(ctx) }()
	go func() { defer wg.Done(); tee.Run(ctx, st) }()
	a := api.New(cfg, st, eng, m, log, "test")
	a.SetLogRing(tee.Ring())
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(func() { srv.Close(); cancel(); wg.Wait() })
	return srv
}
