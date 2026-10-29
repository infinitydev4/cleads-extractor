package obs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/cleads/business-extractor/internal/domain"
)

// Sink stores events. store.Store implements it.
type Sink interface {
	InsertEvents(ctx context.Context, events []domain.LogEvent) error
	PurgeEvents(ctx context.Context, before time.Time) (int64, error)
}

type TeeOptions struct {
	// Worker identifies this process in stored events.
	Worker string
	// MinLevel is the lowest level copied to the sink (default WARN).
	MinLevel slog.Level
	// Buffer is the number of events queued before new ones are dropped
	// (default 4096). The caller never waits for the database.
	Buffer int
	// Batch is the maximum number of events per insert (default 256).
	Batch int
	// FlushEvery bounds how long an event waits in the queue (default 1s).
	FlushEvery time.Duration
	// Retention purges older events (and worker heartbeats); 0 disables it.
	Retention time.Duration
	// PurgeEvery is the purge period (default 1h).
	PurgeEvery time.Duration
	// RingSize keeps the last INFO+ records of this process in memory
	// (0 disables the ring).
	RingSize int
	Redactor *Redactor
	// Events counts events by outcome: stored, dropped (queue full), failed
	// (insert error). Optional.
	Events *prometheus.CounterVec
}

func (o *TeeOptions) defaults() {
	if o.MinLevel == 0 {
		o.MinLevel = slog.LevelWarn
	}
	if o.Buffer <= 0 {
		o.Buffer = 4096
	}
	if o.Batch <= 0 {
		o.Batch = 256
	}
	if o.FlushEvery <= 0 {
		o.FlushEvery = time.Second
	}
	if o.PurgeEvery <= 0 {
		o.PurgeEvery = time.Hour
	}
	if o.Redactor == nil {
		o.Redactor = NewRedactor()
	}
}

// Tee is a slog.Handler that writes to an inner handler (stdout) and copies
// records at or above MinLevel to a Sink, asynchronously and in batches.
// When the queue is full, events are dropped and counted: logging never
// slows the engine down.
type Tee struct {
	inner  slog.Handler
	core   *teeCore
	attrs  []slog.Attr // WithAttrs, keys already qualified by groups
	prefix string      // WithGroup, "a.b."
}

type teeCore struct {
	opts                    TeeOptions
	ch                      chan domain.LogEvent
	ring                    *Ring
	stored, dropped, failed prometheus.Counter
	runOnce                 sync.Once
	lastErrMu               sync.Mutex
	lastErr                 time.Time
}

// NewTee wraps inner. Call Run to start delivering events to a sink; until
// then they wait in the queue.
func NewTee(inner slog.Handler, opts TeeOptions) *Tee {
	opts.defaults()
	c := &teeCore{opts: opts, ch: make(chan domain.LogEvent, opts.Buffer)}
	if opts.RingSize > 0 {
		c.ring = newRing(opts.RingSize, opts.Redactor, opts.Worker)
	}
	if opts.Events != nil {
		c.stored = opts.Events.WithLabelValues("stored")
		c.dropped = opts.Events.WithLabelValues("dropped")
		c.failed = opts.Events.WithLabelValues("failed")
	}
	return &Tee{inner: inner, core: c}
}

// Ring returns the in-memory ring of recent records, or nil.
func (t *Tee) Ring() *Ring { return t.core.ring }

func (t *Tee) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= t.core.opts.MinLevel || (t.core.ring != nil && l >= slog.LevelInfo) || t.inner.Enabled(ctx, l)
}

func (t *Tee) Handle(ctx context.Context, r slog.Record) error {
	var err error
	if t.inner.Enabled(ctx, r.Level) {
		err = t.inner.Handle(ctx, r)
	}
	c := t.core
	if c.ring != nil && r.Level >= slog.LevelInfo {
		c.ring.add(r, t.attrs, t.prefix)
	}
	if r.Level >= c.opts.MinLevel {
		ev := buildEvent(r, t.attrs, t.prefix, c.opts.Redactor)
		ev.Worker = c.opts.Worker
		select {
		case c.ch <- ev:
		default:
			if c.dropped != nil {
				c.dropped.Inc()
			}
		}
	}
	return err
}

func (t *Tee) WithAttrs(as []slog.Attr) slog.Handler {
	if len(as) == 0 {
		return t
	}
	cp := *t
	cp.inner = t.inner.WithAttrs(as)
	cp.attrs = append(append([]slog.Attr(nil), t.attrs...), qualify(as, t.prefix)...)
	return &cp
}

func (t *Tee) WithGroup(name string) slog.Handler {
	if name == "" {
		return t
	}
	cp := *t
	cp.inner = t.inner.WithGroup(name)
	cp.prefix = t.prefix + name + "."
	return &cp
}

func qualify(as []slog.Attr, prefix string) []slog.Attr {
	if prefix == "" {
		return as
	}
	out := make([]slog.Attr, len(as))
	for i, a := range as {
		out[i] = slog.Attr{Key: prefix + a.Key, Value: a.Value}
	}
	return out
}

// Run delivers queued events to sink until ctx is cancelled, then flushes
// what is left (bounded by a few seconds). It also purges old events when
// Retention is set. Only the first call runs; later calls return at once.
func (t *Tee) Run(ctx context.Context, sink Sink) {
	t.core.runOnce.Do(func() { t.core.run(ctx, sink, t.inner) })
}

func (c *teeCore) run(ctx context.Context, sink Sink, inner slog.Handler) {
	flush := time.NewTicker(c.opts.FlushEvery)
	defer flush.Stop()
	purge := time.NewTicker(c.opts.PurgeEvery)
	defer purge.Stop()
	batch := make([]domain.LogEvent, 0, c.opts.Batch)
	write := func(ctx context.Context) {
		if len(batch) == 0 {
			return
		}
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := sink.InsertEvents(wctx, batch)
		cancel()
		if err != nil {
			if c.failed != nil {
				c.failed.Add(float64(len(batch)))
			}
			c.report(inner, "log events not stored", err, len(batch))
		} else if c.stored != nil {
			c.stored.Add(float64(len(batch)))
		}
		batch = batch[:0]
	}
	if c.opts.Retention > 0 {
		c.purge(ctx, sink, inner)
	}
	for {
		select {
		case <-ctx.Done():
			// Final drain, so a shutdown does not lose the last errors.
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer cancel()
			for {
				select {
				case ev := <-c.ch:
					batch = append(batch, ev)
					if len(batch) == cap(batch) {
						write(fctx)
					}
				default:
					write(fctx)
					return
				}
			}
		case ev := <-c.ch:
			batch = append(batch, ev)
			if len(batch) == cap(batch) {
				write(ctx)
			}
		case <-flush.C:
			write(ctx)
		case <-purge.C:
			if c.opts.Retention > 0 {
				c.purge(ctx, sink, inner)
			}
		}
	}
}

func (c *teeCore) purge(ctx context.Context, sink Sink, inner slog.Handler) {
	pctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if _, err := sink.PurgeEvents(pctx, time.Now().Add(-c.opts.Retention)); err != nil && ctx.Err() == nil {
		c.report(inner, "log events purge failed", err, 0)
	}
}

// report writes a sink failure to the inner handler only (going through the
// tee would queue another event for the failing sink), at most once a minute.
func (c *teeCore) report(inner slog.Handler, msg string, err error, n int) {
	if errors.Is(err, context.Canceled) {
		return
	}
	c.lastErrMu.Lock()
	if time.Since(c.lastErr) < time.Minute {
		c.lastErrMu.Unlock()
		return
	}
	c.lastErr = time.Now()
	c.lastErrMu.Unlock()
	r := slog.NewRecord(time.Now(), slog.LevelError, msg, 0)
	r.AddAttrs(slog.String("err", c.opts.Redactor.String(err.Error())), slog.Int("events", n))
	_ = inner.Handle(context.Background(), r)
}

// ---------------------------------------------------------------- records → events

const (
	maxAttrs    = 32
	maxValueLen = 2048
)

func buildEvent(r slog.Record, preset []slog.Attr, prefix string, red *Redactor) domain.LogEvent {
	ev := domain.LogEvent{Time: r.Time.UTC(), Level: r.Level.String(), Msg: truncate(red.String(r.Message))}
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	add := func(key string, v slog.Value) {
		v = v.Resolve()
		switch key {
		case "job", "job_id":
			if ev.JobID == "" {
				ev.JobID = v.String()
				return
			}
		case "task", "task_id":
			if ev.TaskID == 0 {
				if n, ok := toInt64(v); ok {
					ev.TaskID = n
					return
				}
			}
		case "source", "queue":
			if ev.Source == "" {
				ev.Source = v.String()
				return
			}
		case "worker":
			if ev.Worker == "" {
				ev.Worker = v.String()
				return
			}
		}
		if ev.Attrs == nil {
			ev.Attrs = map[string]any{}
		}
		if len(ev.Attrs) < maxAttrs {
			if secretKey(key) {
				ev.Attrs[key] = masked
			} else {
				ev.Attrs[key] = jsonValue(v, red)
			}
		}
	}
	for _, a := range preset {
		add(a.Key, a.Value)
	}
	r.Attrs(func(a slog.Attr) bool {
		add(prefix+a.Key, a.Value)
		return true
	})
	return ev
}

func toInt64(v slog.Value) (int64, bool) {
	switch v.Kind() {
	case slog.KindInt64:
		return v.Int64(), true
	case slog.KindUint64:
		return int64(v.Uint64()), true
	case slog.KindString:
		n, err := strconv.ParseInt(v.String(), 10, 64)
		return n, err == nil
	}
	return 0, false
}

// jsonValue converts a slog value to something encoding/json (and jsonb)
// accepts, with secrets masked and long strings truncated.
func jsonValue(v slog.Value, red *Redactor) any {
	switch v.Kind() {
	case slog.KindString:
		return truncate(red.String(v.String()))
	case slog.KindInt64:
		return v.Int64()
	case slog.KindUint64:
		return v.Uint64()
	case slog.KindFloat64:
		return v.Float64()
	case slog.KindBool:
		return v.Bool()
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time().UTC().Format(time.RFC3339Nano)
	case slog.KindGroup:
		m := map[string]any{}
		for _, a := range v.Group() {
			if secretKey(a.Key) {
				m[a.Key] = masked
				continue
			}
			m[a.Key] = jsonValue(a.Value.Resolve(), red)
		}
		return m
	}
	switch x := v.Any().(type) {
	case error:
		return truncate(red.String(x.Error()))
	case fmt.Stringer:
		return truncate(red.String(x.String()))
	case nil:
		return nil
	default:
		// Round-trip through JSON so the value is plain maps and slices that
		// the redactor can walk.
		b, err := json.Marshal(x)
		if err != nil || len(b) > maxValueLen*4 {
			return truncate(red.String(fmt.Sprint(x)))
		}
		var out any
		if err := json.Unmarshal(b, &out); err != nil {
			return truncate(red.String(string(b)))
		}
		return red.Value(out)
	}
}

func truncate(s string) string {
	if len(s) <= maxValueLen {
		return s
	}
	cut := maxValueLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// ---------------------------------------------------------------- ring

// Ring keeps the most recent records of this process in memory, for local
// debugging (records are converted and redacted only when read).
type Ring struct {
	mu     sync.Mutex
	buf    []ringEntry
	next   int
	full   bool
	red    *Redactor
	worker string
}

type ringEntry struct {
	r      slog.Record
	preset []slog.Attr
	prefix string
}

func newRing(n int, red *Redactor, worker string) *Ring {
	return &Ring{buf: make([]ringEntry, n), red: red, worker: worker}
}

func (g *Ring) add(r slog.Record, preset []slog.Attr, prefix string) {
	e := ringEntry{r: r.Clone(), preset: preset, prefix: prefix}
	g.mu.Lock()
	g.buf[g.next] = e
	g.next++
	if g.next == len(g.buf) {
		g.next, g.full = 0, true
	}
	g.mu.Unlock()
}

// Snapshot returns up to limit records, newest first, at or above minLevel
// and matching jobID / source when set.
func (g *Ring) Snapshot(minLevel slog.Level, jobID, source string, limit int) []domain.LogEvent {
	g.mu.Lock()
	n := g.next
	if g.full {
		n = len(g.buf)
	}
	entries := make([]ringEntry, 0, n)
	for i := 1; i <= n; i++ {
		entries = append(entries, g.buf[(g.next-i+len(g.buf))%len(g.buf)])
	}
	g.mu.Unlock()
	var out []domain.LogEvent
	for _, e := range entries {
		if len(out) >= limit {
			break
		}
		if e.r.Level < minLevel {
			continue
		}
		ev := buildEvent(e.r, e.preset, e.prefix, g.red)
		if (jobID != "" && ev.JobID != jobID) || (source != "" && !strings.HasPrefix(ev.Source, source)) {
			continue
		}
		if ev.Worker == "" {
			ev.Worker = g.worker
		}
		out = append(out, ev)
	}
	return out
}
