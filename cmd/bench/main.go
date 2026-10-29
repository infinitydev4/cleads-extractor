// Command bench measures end-to-end engine throughput with the synthetic
// source, so that capacity claims come from measurements, not estimates.
//
// It isolates the ceiling of our own code (planning, normalization, dedup,
// storage, queue) from provider limits: with -latency you emulate network
// round trips, with -workers you emulate several worker replicas sharing the
// same PostgreSQL. Real throughput is then min(this ceiling, provider quotas).
//
//	go run ./cmd/bench -store pg -dsn postgres://... -workers 3 -area-km 30
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/engine"
	"github.com/cleads/business-extractor/internal/geo"
	"github.com/cleads/business-extractor/internal/geocode"
	"github.com/cleads/business-extractor/internal/metrics"
	"github.com/cleads/business-extractor/internal/source"
	"github.com/cleads/business-extractor/internal/source/mock"
	"github.com/cleads/business-extractor/internal/store"
	"github.com/cleads/business-extractor/internal/store/memstore"
	"github.com/cleads/business-extractor/internal/store/pgstore"
)

type report struct {
	Store         string  `json:"store"`
	Workers       int     `json:"workers"`
	Concurrency   int     `json:"concurrency_per_worker"`
	LatencyMS     int64   `json:"source_latency_ms"`
	AreaKm        float64 `json:"area_km"`
	CellKm        float64 `json:"cell_km"`
	Seconds       float64 `json:"seconds"`
	Tasks         int64   `json:"tasks"`
	Raw           int64   `json:"raw"`
	Unique        int64   `json:"unique"`
	Complete      int64   `json:"complete"`
	CRMUseful     int64   `json:"crm_useful"`
	DupRatio      float64 `json:"duplicate_ratio"`
	RawPerSec     float64 `json:"raw_per_sec"`
	UniquePerSec  float64 `json:"unique_per_sec"`
	UsefulPerSec  float64 `json:"crm_useful_per_sec"`
	UniquePerDay  float64 `json:"unique_per_day_at_this_rate"`
	UsefulPerDay  float64 `json:"crm_useful_per_day_at_this_rate"`
	TasksPerSec   float64 `json:"tasks_per_sec"`
	MaxRSSMB      float64 `json:"max_rss_mb"`
	HeapInuseMB   float64 `json:"heap_inuse_mb_end"`
	GoroutinesEnd int     `json:"goroutines_end"`
	NumCPU        int     `json:"num_cpu"`
	GOOS          string  `json:"goos"`
	GOARCH        string  `json:"goarch"`
}

func main() {
	var (
		storeKind = flag.String("store", "mem", "mem | pg")
		dsn       = flag.String("dsn", os.Getenv("DATABASE_URL"), "PostgreSQL DSN for -store pg")
		workers   = flag.Int("workers", 1, "engine instances (emulated replicas)")
		conc      = flag.Int("concurrency", 32, "source concurrency per worker")
		latency   = flag.Duration("latency", 150*time.Millisecond, "mean simulated source latency")
		areaKm    = flag.Float64("area-km", 20, "side of the square area")
		cellKm    = flag.Float64("cell-km", 1, "grid cell size")
		density   = flag.Float64("density", 40, "businesses per km²")
		pages     = flag.Int("pages", 3, "pages per query (20 results each)")
		failRatio = flag.Float64("fail", 0.02, "transient failure probability")
		jsonOut   = flag.Bool("json", false, "print JSON only")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	var st store.Store
	switch *storeKind {
	case "mem":
		if *workers > 1 {
			fmt.Fprintln(os.Stderr, "-workers > 1 requires -store pg (memstore is per process)")
			os.Exit(2)
		}
		st = memstore.New()
	case "pg":
		pg, err := pgstore.Open(ctx, *dsn, int32(8**workers+8), log)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer pg.Close()
		st = pg
	default:
		fmt.Fprintln(os.Stderr, "unknown -store")
		os.Exit(2)
	}

	center := domain.Point{Lat: 45.76, Lng: 4.84}
	area := geo.BBoxAround(center, *areaKm*1000/2)
	m := metrics.New()
	runCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for w := 0; w < *workers; w++ {
		src := mock.New(mock.Config{Latency: *latency, DensityKm2: *density, Pages: *pages, CellKm: *cellKm,
			FailRatio: *failRatio, Concurrency: *conc})
		e := engine.New(engine.Config{PollMin: 10 * time.Millisecond, PollMax: 200 * time.Millisecond,
			FinalizeEvery: 100 * time.Millisecond, MinRetryAfter: 100 * time.Millisecond, DisableFinalize: w > 0},
			st, []source.Source{src}, geocode.Static{"bench": area}, nil, m, log)
		wg.Add(1)
		go func() { defer wg.Done(); _ = e.Run(runCtx) }()
	}

	spec := domain.JobSpec{Query: "plombier", Location: "bench"}
	spec.Normalize()
	job := domain.Job{ID: fmt.Sprintf("bench_%d", time.Now().UnixNano()), Spec: spec, Status: domain.JobQueued, CreatedAt: time.Now()}
	start := time.Now()
	if err := st.CreateJob(ctx, &job, []domain.Task{engine.PlanTask(&job)}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	last := time.Now()
	for {
		time.Sleep(100 * time.Millisecond)
		j, err := st.GetJob(ctx, job.ID)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if !*jsonOut && time.Since(last) > 2*time.Second {
			last = time.Now()
			fmt.Fprintf(os.Stderr, "  %5.1fs  tasks %d/%d  raw %d  unique %d\n", time.Since(start).Seconds(),
				j.Counters.TasksDone, j.Counters.TasksTotal, j.Counters.RawFound, j.Counters.Unique)
		}
		if j.Status.Terminal() {
			job = j
			break
		}
	}
	elapsed := time.Since(start).Seconds()
	cancel()
	wg.Wait()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	maxRSS := float64(ru.Maxrss)
	if runtime.GOOS == "darwin" {
		maxRSS /= 1 << 20 // bytes on macOS
	} else {
		maxRSS /= 1 << 10 // kilobytes on Linux
	}
	c := job.Counters
	r := report{
		Store: *storeKind, Workers: *workers, Concurrency: *conc, LatencyMS: latency.Milliseconds(),
		AreaKm: *areaKm, CellKm: *cellKm, Seconds: round(elapsed), Tasks: c.TasksDone + c.TasksFailed,
		Raw: c.RawFound, Unique: c.Unique, Complete: c.Complete, CRMUseful: c.CRMUseful,
		RawPerSec: round(float64(c.RawFound) / elapsed), UniquePerSec: round(float64(c.Unique) / elapsed),
		UsefulPerSec: round(float64(c.CRMUseful) / elapsed), TasksPerSec: round(float64(c.TasksDone) / elapsed),
		UniquePerDay: float64(int64(float64(c.Unique) / elapsed * 86400)), UsefulPerDay: float64(int64(float64(c.CRMUseful) / elapsed * 86400)),
		MaxRSSMB: round(maxRSS), HeapInuseMB: round(float64(ms.HeapInuse) / (1 << 20)), GoroutinesEnd: runtime.NumGoroutine(),
		NumCPU: runtime.NumCPU(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
	}
	if c.RawFound > 0 {
		r.DupRatio = round(1 - float64(c.Unique)/float64(c.RawFound))
	}
	out, _ := json.MarshalIndent(r, "", "  ")
	fmt.Println(string(out))
}

func round(v float64) float64 { return float64(int64(v*100)) / 100 }
