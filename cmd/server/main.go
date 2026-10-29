// Command server runs the CLEADS Business Extractor. ROLE selects what this
// process does: "api" (HTTP only), "worker" (extraction only) or "all".
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	_ "net/http/pprof" //nolint:gosec // served only on PPROF_ADDR when set
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cleads/business-extractor/internal/api"
	"github.com/cleads/business-extractor/internal/config"
	"github.com/cleads/business-extractor/internal/engine"
	"github.com/cleads/business-extractor/internal/enrich"
	"github.com/cleads/business-extractor/internal/geocode"
	"github.com/cleads/business-extractor/internal/mcpserver"
	"github.com/cleads/business-extractor/internal/metrics"
	"github.com/cleads/business-extractor/internal/obs"
	"github.com/cleads/business-extractor/internal/source"
	"github.com/cleads/business-extractor/internal/source/gmapsbrowser"
	"github.com/cleads/business-extractor/internal/source/gmapshttp"
	"github.com/cleads/business-extractor/internal/source/mock"
	"github.com/cleads/business-extractor/internal/source/osm"
	"github.com/cleads/business-extractor/internal/source/placesapi"
	"github.com/cleads/business-extractor/internal/source/sirene"
	"github.com/cleads/business-extractor/internal/store"
	"github.com/cleads/business-extractor/internal/store/memstore"
	"github.com/cleads/business-extractor/internal/store/pgstore"
)

var version = "dev"

func main() {
	// "cleads-extractor mcp": stdio MCP server driving a remote instance
	// through CLEADS_URL / CLEADS_API_KEY (same image, no database).
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		err := mcpserver.RunStdio(ctx, version, os.Getenv, nil, os.Stderr)
		stop()
		if err != nil {
			fmt.Fprintln(os.Stderr, "fatal:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(os.Args[2:], os.Getenv, os.Stderr))
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	m := metrics.New()
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(cfg.LogLevel))
	// Warnings and errors are also copied to the events table (readable by
	// any API replica); the copy is asynchronous and drops under pressure.
	log, tee := obs.NewLogger(os.Stdout, cfg.LogFormat, lvl, obs.TeeOptions{
		Worker: cfg.WorkerID, Buffer: cfg.EventsBuffer, Retention: cfg.EventsRetention, RingSize: cfg.LogRingSize,
		Redactor: obs.NewRedactor(cfg.Secrets...), Events: m.LogEvents,
	})
	slog.SetDefault(log)
	if lim := applyMemoryLimit(); lim > 0 {
		log.Info("memory limit set from cgroup", "gomemlimit_mb", lim>>20)
	}
	log.Info("starting", "version", version, "config", cfg.Redacted())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var st store.Store
	if cfg.DatabaseURL != "" {
		pg, err := pgstore.OpenWait(ctx, cfg.DatabaseURL, cfg.DBMaxConns, cfg.DBStartupWait, log)
		if err != nil {
			return err
		}
		st = pg
	} else {
		log.Warn("DATABASE_URL not set: using the in-memory store (data is lost on restart, single replica only)")
		st = memstore.New()
	}
	defer st.Close()
	teeCtx, stopTee := context.WithCancel(context.Background())
	teeDone := make(chan struct{})
	go func() { tee.Run(teeCtx, st); close(teeDone) }()
	defer func() { stopTee(); <-teeDone }() // flush before the store closes

	sources, err := buildSources(cfg, log)
	if err != nil {
		return err
	}
	if len(sources) == 0 {
		return errors.New("no source enabled: set ENABLE_GMAPS_SCRAPING, GOOGLE_PLACES_API_KEY, ENABLE_OSM, ENABLE_SIRENE or ENABLE_MOCK_SOURCE")
	}
	var gc geocode.Geocoder
	switch cfg.Geocoder {
	case "google":
		gc = geocode.NewCached(geocode.NewGoogle(cfg.PlacesAPIKey), 24*time.Hour)
	case "nominatim":
		gc = geocode.NewCached(geocode.NewNominatim(cfg.NominatimURL, cfg.GeocoderUA), 24*time.Hour)
	}
	var crawler *enrich.Crawler
	if cfg.EnrichEnabled {
		crawler = enrich.New(enrich.Config{MaxPages: cfg.EnrichMaxPages})
	}
	eng := engine.New(engine.Config{
		Lease: cfg.Lease, PlanConcurrency: cfg.PlanConcurrency, EnrichConcurrency: cfg.EnrichConc,
		SourceConcurrency: cfg.SourceConc, SourceRate: cfg.SourceRate, DedupCacheSize: cfg.DedupCacheSize,
		MaxCellsPerJob: cfg.MaxCellsPerJob, DrainTimeout: cfg.ShutdownWindow - 5*time.Second,
		WebhookSecret: cfg.WebhookSecret, DisableDetails: !cfg.GMapsDetails,
		WorkerID: cfg.WorkerID, Role: string(cfg.Role), Version: version,
	}, st, sources, gc, crawler, m, log)

	if cfg.PprofAddr != "" {
		go func() {
			log.Info("pprof listening", "addr", cfg.PprofAddr)
			srv := &http.Server{Addr: cfg.PprofAddr, Handler: http.DefaultServeMux, ReadHeaderTimeout: 5 * time.Second}
			_ = srv.ListenAndServe()
		}()
	}

	var wg sync.WaitGroup
	errc := make(chan error, 2)

	if cfg.Role == config.RoleAll || cfg.Role == config.RoleWorker {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := eng.Run(ctx); err != nil {
				errc <- fmt.Errorf("engine: %w", err)
			}
		}()
	}

	// Workers also listen on PORT so Railway health checks and Prometheus
	// scraping work for every role.
	apiSrv := api.New(cfg, st, eng, m, log, version)
	apiSrv.SetLogRing(tee.Ring())
	handler := apiSrv.Handler()
	if cfg.Role == config.RoleWorker {
		handler = workerHandler(handler)
	}
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second, // streaming endpoints lift it per request
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	go func() {
		log.Info("http listening", "addr", srv.Addr, "role", cfg.Role)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("http: %w", err)
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-errc:
		log.Error("component failed", "err", err)
		stop()
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownWindow)
	defer cancel()
	_ = srv.Shutdown(sctx)
	wg.Wait()
	log.Info("stopped")
	return nil
}

// workerHandler exposes only health and metrics on worker replicas.
func workerHandler(full http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/healthz", r.URL.Path == "/readyz", r.URL.Path == "/metrics":
			full.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

func buildSources(cfg *config.Config, log *slog.Logger) ([]source.Source, error) {
	var out []source.Source
	if cfg.PlacesAPIKey != "" {
		s, err := placesapi.New(placesapi.Config{APIKey: cfg.PlacesAPIKey, FieldTier: cfg.PlacesFieldTier})
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if cfg.EnableGMapsHTTP {
		proxies, err := source.ParseProxies(cfg.GMapsProxies)
		if err != nil {
			return nil, err
		}
		log.Info("google_maps_http enabled (default Google source, no API key)", "details", cfg.GMapsDetails,
			"proxies", len(proxies))
		out = append(out, gmapshttp.New(gmapshttp.Config{
			HTTPClient: source.NewHTTPClient(15*time.Second, 64, proxies), MaxPages: cfg.GMapsMaxPages,
			DebugDir: cfg.GMapsDebugDir,
		}))
	}
	if cfg.GMapsScraperURL != "" {
		s, err := gmapsbrowser.New(gmapsbrowser.Config{BaseURL: cfg.GMapsScraperURL, ExtractEmail: cfg.GMapsScraperEmail})
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if cfg.EnableOSM {
		out = append(out, osm.New(osm.Config{Endpoints: cfg.OverpassURLs}))
	}
	if cfg.EnableSirene {
		out = append(out, sirene.New(sirene.Config{Endpoint: cfg.SireneURL}))
	}
	if cfg.EnableMock {
		out = append(out, mock.New(mock.Config{Latency: 50 * time.Millisecond}))
	}
	return out, nil
}

// applyMemoryLimit sets GOMEMLIMIT to 90% of the container memory limit when
// the operator did not set it, so the GC works harder before the OOM killer
// steps in. Returns the limit applied, or 0.
func applyMemoryLimit() int64 {
	if os.Getenv("GOMEMLIMIT") != "" {
		return 0
	}
	for _, p := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		if err != nil || v <= 0 || v > 1<<50 {
			return 0
		}
		lim := v / 10 * 9
		debug.SetMemoryLimit(lim)
		return lim
	}
	return 0
}
