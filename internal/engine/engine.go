// Package engine runs the extraction workers.
//
// Concurrency model: one bounded pool per source (its own concurrency, rate
// governor and circuit breaker), plus a planner pool and an enrichment pool.
// Pools lease tasks from the shared queue in batches sized to their free
// slots, so a slow or blocked source never starves the others, and any number
// of worker replicas can run against the same database.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cleads/business-extractor/internal/dedup"
	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/enrich"
	"github.com/cleads/business-extractor/internal/geocode"
	"github.com/cleads/business-extractor/internal/metrics"
	"github.com/cleads/business-extractor/internal/ratelimit"
	"github.com/cleads/business-extractor/internal/source"
	"github.com/cleads/business-extractor/internal/store"
)

// Internal queue names for non-source work.
const (
	QueuePlan   = "_plan"
	QueueEnrich = "_enrich"
)

type Config struct {
	Lease             time.Duration
	PollMin           time.Duration
	PollMax           time.Duration
	MaxAttempts       int
	PlanConcurrency   int
	EnrichConcurrency int
	// SourceConcurrency overrides Info().Concurrency per source.
	SourceConcurrency map[string]int
	// SourceRate overrides Info().RatePerSec per source.
	SourceRate      map[string]float64
	DedupCacheSize  int
	MaxCellsPerJob  int
	DrainTimeout    time.Duration
	FinalizeEvery   time.Duration
	WebhookSecret   string
	DisableFinalize bool
	// MinRetryAfter floors provider retry hints (default 5s).
	MinRetryAfter time.Duration
	// DisableDetails skips full place profiles for sources that offer them.
	DisableDetails bool
	// WorkerID, Role and Version identify this process in its heartbeat
	// (table workers), published every StatusEvery (default 10s).
	WorkerID    string
	Role        string
	Version     string
	StatusEvery time.Duration
}

// DetailsQueue is the queue name of a source's place-details tasks.
func DetailsQueue(source string) string { return source + ".details" }

func (c *Config) defaults() {
	if c.Lease == 0 {
		c.Lease = 2 * time.Minute
	}
	if c.PollMin == 0 {
		c.PollMin = 50 * time.Millisecond
	}
	if c.PollMax == 0 {
		c.PollMax = time.Second
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 5
	}
	if c.PlanConcurrency == 0 {
		c.PlanConcurrency = 4
	}
	if c.EnrichConcurrency == 0 {
		c.EnrichConcurrency = 32
	}
	if c.DedupCacheSize == 0 {
		c.DedupCacheSize = 2_000_000
	}
	if c.MaxCellsPerJob == 0 {
		c.MaxCellsPerJob = 2_500
	}
	if c.DrainTimeout == 0 {
		c.DrainTimeout = 20 * time.Second
	}
	if c.MinRetryAfter == 0 {
		c.MinRetryAfter = 5 * time.Second
	}
	if c.FinalizeEvery == 0 {
		c.FinalizeEvery = 2 * time.Second
	}
	if c.StatusEvery == 0 {
		c.StatusEvery = 10 * time.Second
	}
	if c.Role == "" {
		c.Role = "worker"
	}
}

type Engine struct {
	cfg      Config
	store    store.Store
	sources  map[string]source.Source
	details  map[string]source.Detailer // by source name
	govs     map[string]*ratelimit.Governor
	geocoder geocode.Geocoder
	crawler  *enrich.Crawler
	cache    *dedup.Cache
	m        *metrics.Metrics
	log      *slog.Logger
	hooks    *webhooks
	window   *outcomeWindow
	inflight map[string]*atomic.Int64 // by queue
	started  time.Time                // immutable after New
}

func New(cfg Config, st store.Store, sources []source.Source, gc geocode.Geocoder, cr *enrich.Crawler,
	m *metrics.Metrics, log *slog.Logger) *Engine {
	cfg.defaults()
	e := &Engine{
		cfg: cfg, store: st, geocoder: gc, crawler: cr, m: m, log: log,
		sources: map[string]source.Source{}, govs: map[string]*ratelimit.Governor{}, details: map[string]source.Detailer{},
		cache:  dedup.NewCache(cfg.DedupCacheSize),
		hooks:  newWebhooks(cfg.WebhookSecret, log),
		window: newOutcomeWindow(), inflight: map[string]*atomic.Int64{}, started: time.Now().UTC(),
	}
	for _, q := range []string{QueuePlan, QueueEnrich} {
		e.inflight[q] = &atomic.Int64{}
	}
	for _, s := range sources {
		info := s.Info()
		r, burst := info.RatePerSec, info.Burst
		if v, ok := cfg.SourceRate[info.Name]; ok && v > 0 {
			r, burst = v, max(1, int(v))
		}
		e.sources[info.Name] = s
		e.govs[info.Name] = ratelimit.NewGovernor(r, burst)
		e.inflight[info.Name] = &atomic.Int64{}
		if d, ok := s.(source.Detailer); ok && !cfg.DisableDetails {
			e.details[info.Name] = d
			e.inflight[DetailsQueue(info.Name)] = &atomic.Int64{}
		}
	}
	return e
}

// Sources returns the enabled sources' descriptions.
func (e *Engine) Sources() []source.Info {
	out := make([]source.Info, 0, len(e.sources))
	for _, s := range e.sources {
		out = append(out, s.Info())
	}
	return out
}

// HasSource reports whether name is enabled.
func (e *Engine) HasSource(name string) bool { _, ok := e.sources[name]; return ok }

type execFn func(ctx context.Context, t *domain.Task) store.TaskOutcome

// Run blocks until ctx is cancelled, then drains in-flight tasks.
func (e *Engine) Run(ctx context.Context) error {
	// work outlives ctx by DrainTimeout so in-flight tasks can finish.
	work, cancelWork := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelWork()

	var wg sync.WaitGroup
	start := func(queue string, n int, fn execFn) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.runPool(ctx, work, queue, n, fn)
		}()
	}
	start(QueuePlan, e.cfg.PlanConcurrency, e.execPlan)
	for name, s := range e.sources {
		n := s.Info().Concurrency
		if v, ok := e.cfg.SourceConcurrency[name]; ok && v > 0 {
			n = v
		}
		start(name, max(1, n), e.execSearch)
		if _, ok := e.details[name]; ok {
			start(DetailsQueue(name), max(1, n), e.execDetails)
		}
	}
	if e.crawler != nil {
		start(QueueEnrich, e.cfg.EnrichConcurrency, e.execEnrich)
	}
	if !e.cfg.DisableFinalize {
		wg.Add(1)
		go func() { defer wg.Done(); e.finalizeLoop(ctx) }()
	}
	wg.Add(1)
	go func() { defer wg.Done(); e.gaugeLoop(ctx) }()

	e.log.Info("engine started", "sources", len(e.sources), "enrich", e.crawler != nil, "worker", e.cfg.WorkerID)
	<-ctx.Done()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(e.cfg.DrainTimeout):
		e.log.Warn("drain timeout: abandoning in-flight tasks (they will be re-leased)")
		cancelWork()
		<-done
	}
	e.hooks.wait(5 * time.Second)
	return nil
}

func (e *Engine) runPool(ctx, work context.Context, queue string, concurrency int, fn execFn) {
	sem := make(chan struct{}, concurrency)
	freed := make(chan struct{}, 1)
	var wg sync.WaitGroup
	defer wg.Wait()
	backoff := e.cfg.PollMin
	gauge := e.m.TasksInFlight.WithLabelValues(queue)
	inflight := e.inflight[queue]
	for ctx.Err() == nil {
		free := concurrency - len(sem)
		if free <= 0 {
			select {
			case <-ctx.Done():
				return
			case <-freed:
			}
			continue
		}
		tasks, err := e.store.LeaseTasks(ctx, queue, free, e.cfg.Lease)
		if err != nil {
			if ctx.Err() == nil {
				e.log.Error("lease failed", "queue", queue, "err", err)
			}
			sleep(ctx, e.cfg.PollMax)
			continue
		}
		if len(tasks) == 0 {
			sleep(ctx, backoff)
			backoff = min(backoff*2, e.cfg.PollMax)
			continue
		}
		backoff = e.cfg.PollMin
		for i := range tasks {
			t := tasks[i]
			sem <- struct{}{}
			gauge.Inc()
			inflight.Add(1)
			wg.Add(1)
			go func() {
				defer func() {
					gauge.Dec()
					inflight.Add(-1)
					<-sem
					select {
					case freed <- struct{}{}:
					default:
					}
					wg.Done()
				}()
				e.runTask(work, &t, fn)
			}()
		}
	}
}

func (e *Engine) runTask(ctx context.Context, t *domain.Task, fn execFn) {
	hbCtx, stopHB := context.WithCancel(ctx)
	go e.heartbeat(hbCtx, t.ID)
	var out store.TaskOutcome
	func() {
		defer func() {
			if r := recover(); r != nil {
				e.log.Error("task panic", "job", t.JobID, "task", t.ID, "queue", t.Source, "panic", r, "stack", string(debug.Stack()))
				out = e.retryOrFail(t, fmt.Errorf("panic: %v", r))
			}
		}()
		out = fn(ctx, t)
	}()
	stopHB()
	if ctx.Err() != nil && !out.Done {
		// shutdown: give the task back immediately
		out = store.TaskOutcome{RetryAt: time.Now(), Err: "worker shutdown", KeepAttempt: true, Delta: out.Delta}
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := e.store.CompleteTask(cctx, t, out); err != nil && !errors.Is(err, store.ErrConflict) {
		e.log.Error("complete task failed", "job", t.JobID, "task", t.ID, "source", t.Source, "err", err)
		return
	}
	if !out.Done && out.RetryAt.IsZero() {
		// Final failure: the task stays in the table (status failed) and this
		// event lets the API explain it, whichever worker ran it.
		e.log.Warn("task failed", "job", t.JobID, "task", t.ID, "source", t.Source, "kind", t.Kind,
			"attempts", t.Attempts, "err", out.Err)
	}
}

func (e *Engine) heartbeat(ctx context.Context, id int64) {
	t := time.NewTicker(e.cfg.Lease / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := e.store.ExtendLease(ctx, []int64{id}, e.cfg.Lease); err != nil && ctx.Err() == nil {
				e.log.Warn("extend lease failed", "task", id, "err", err)
			}
		}
	}
}

// retryOrFail applies exponential backoff with jitter, then gives up.
func (e *Engine) retryOrFail(t *domain.Task, err error) store.TaskOutcome {
	if t.Attempts >= e.cfg.MaxAttempts {
		return store.TaskOutcome{Err: err.Error()}
	}
	d := time.Duration(1<<min(t.Attempts, 8)) * time.Second
	d += time.Duration(rand.Int64N(int64(d/2) + 1))
	return store.TaskOutcome{RetryAt: time.Now().Add(d), Err: err.Error()}
}

func (e *Engine) finalizeLoop(ctx context.Context) {
	t := time.NewTicker(e.cfg.FinalizeEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			jobs, err := e.store.FinalizeJobs(ctx)
			if err != nil {
				if ctx.Err() == nil {
					e.log.Error("finalize jobs", "err", err)
				}
				continue
			}
			for _, j := range jobs {
				lvl := slog.LevelInfo
				if j.Status == domain.JobFailed {
					lvl = slog.LevelWarn
				}
				e.log.Log(ctx, lvl, "job finished", "error", j.Error, "job", j.ID, "status", j.Status, "unique", j.Counters.Unique,
					"raw", j.Counters.RawFound, "complete", j.Counters.Complete, "crm_useful", j.Counters.CRMUseful)
				if j.Spec.WebhookURL != "" {
					e.hooks.send(j)
				}
			}
		}
	}
}

func (e *Engine) gaugeLoop(ctx context.Context) {
	t := time.NewTicker(e.cfg.StatusEvery)
	defer t.Stop()
	for {
		for name, g := range e.govs {
			e.m.SourceRate.WithLabelValues(name).Set(g.Rate())
			open := 0.0
			if !g.OpenUntil().IsZero() {
				open = 1
			}
			e.m.CircuitOpen.WithLabelValues(name).Set(open)
		}
		if st, err := e.store.Stats(ctx); err == nil {
			for src, n := range st.TasksPending {
				e.m.TasksPending.WithLabelValues(src).Set(float64(n))
			}
		}
		if e.cfg.WorkerID != "" {
			if err := e.store.PutWorkerStatus(ctx, e.Status()); err != nil && ctx.Err() == nil {
				e.log.Warn("publish worker status failed", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// recordOutcome counts a source request in Prometheus and in the recent
// window published for the health summary.
func (e *Engine) recordOutcome(queue, outcome string) {
	e.m.SourceRequests.WithLabelValues(queue, outcome).Inc()
	e.window.add(governorName(queue), outcome, time.Now())
}

// Status is this worker's heartbeat: live governor and breaker state and
// recent request outcomes per source.
func (e *Engine) Status() domain.WorkerStatus {
	now := time.Now().UTC()
	names := make([]string, 0, len(e.govs))
	for n := range e.govs {
		names = append(names, n)
	}
	slices.Sort(names)
	w := domain.WorkerStatus{ID: e.cfg.WorkerID, Role: e.cfg.Role, Version: e.cfg.Version, StartedAt: e.started, SeenAt: now}
	for _, n := range names {
		g := e.govs[n]
		st := domain.SourceStatus{Name: n, Rate: g.Rate(), BaseRate: g.BaseRate(), CircuitOpenUntil: g.OpenUntil(),
			Recent: e.window.sum(n, now), WindowSeconds: windowMinutes * 60}
		for _, q := range []string{n, DetailsQueue(n)} {
			if c := e.inflight[q]; c != nil {
				st.InFlight += c.Load()
			}
		}
		w.Sources = append(w.Sources, st)
	}
	return w
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
