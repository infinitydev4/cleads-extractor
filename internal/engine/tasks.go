package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/cleads/business-extractor/internal/dedup"
	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/geo"
	"github.com/cleads/business-extractor/internal/geocode"
	"github.com/cleads/business-extractor/internal/normalize"
	"github.com/cleads/business-extractor/internal/ratelimit"
	"github.com/cleads/business-extractor/internal/source"
	"github.com/cleads/business-extractor/internal/store"
	"github.com/cleads/business-extractor/internal/taxonomy"
)

// PlanTask returns the initial task of a job.
func PlanTask(job *domain.Job) domain.Task {
	return domain.Task{Kind: domain.TaskPlan, Source: QueuePlan, Priority: job.Spec.Priority + 10}
}

// ResolveSector returns the explicit sector or guesses it from the query.
func ResolveSector(spec *domain.JobSpec) *taxonomy.Sector {
	if spec.Sector != "" {
		return taxonomy.Get(spec.Sector)
	}
	return taxonomy.Guess(spec.Query, normalize.Fold)
}

// SelectSources returns the sources that can serve spec, and why others
// were skipped.
func (e *Engine) SelectSources(spec *domain.JobSpec) (names []string, skipped map[string]string) {
	skipped = map[string]string{}
	want := spec.Sources
	if len(want) == 0 {
		for n := range e.sources {
			want = append(want, n)
		}
		slices.Sort(want)
	}
	sector := ResolveSector(spec)
	for _, n := range want {
		switch {
		case !e.HasSource(n):
			skipped[n] = "source not enabled on this server"
		case n == "sirene" && (sector == nil || len(sector.NAF) == 0):
			skipped[n] = "sirene needs a sector (NAF codes); pass sector or a known activity"
		case n == "sirene" && spec.Country != "FR":
			skipped[n] = "sirene only covers France"
		default:
			names = append(names, n)
		}
	}
	return names, skipped
}

func (e *Engine) execPlan(ctx context.Context, t *domain.Task) store.TaskOutcome {
	job, err := e.store.GetJob(ctx, t.JobID)
	if err != nil {
		return e.retryOrFail(t, err)
	}
	if job.Status.Terminal() {
		return store.TaskOutcome{Done: true}
	}
	_ = e.store.MarkJobRunning(ctx, job.ID)
	spec := job.Spec

	var area domain.BBox
	switch {
	case spec.BBox != nil:
		area = *spec.BBox
	case spec.Center != nil:
		area = geo.BBoxAround(*spec.Center, spec.RadiusM)
	default:
		if e.geocoder == nil {
			_ = e.store.FailJob(ctx, job.ID, "no geocoder configured: pass bbox or center")
			return store.TaskOutcome{Done: true}
		}
		gctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		area, err = e.geocoder.Geocode(gctx, spec.Location, spec.Country, spec.Language)
		cancel()
		if errors.Is(err, geocode.ErrNotFound) {
			_ = e.store.FailJob(ctx, job.ID, err.Error())
			return store.TaskOutcome{Done: true}
		}
		if err != nil {
			return e.retryOrFail(t, fmt.Errorf("geocode: %w", err))
		}
	}

	names, skipped := e.SelectSources(&spec)
	if len(names) == 0 {
		reasons := make([]string, 0, len(skipped))
		for k, v := range skipped {
			reasons = append(reasons, k+": "+v)
		}
		slices.Sort(reasons)
		_ = e.store.FailJob(ctx, job.ID, "no usable source ("+strings.Join(reasons, "; ")+")")
		return store.TaskOutcome{Done: true}
	}
	sector := ResolveSector(&spec)
	sectorKey := ""
	if sector != nil {
		sectorKey = sector.Key
	}
	budget := e.cfg.MaxCellsPerJob / len(names)
	var tasks []domain.Task
	for _, n := range names {
		info := e.sources[n].Info()
		cellKm := spec.GridCellKm
		if cellKm == 0 {
			cellKm = info.DefaultCellKm
		}
		maxCells := min(max(1, info.MaxCells), max(1, budget))
		cells := geo.Grid(area, cellKm, maxCells)
		for i := range cells {
			c := cells[i]
			tasks = append(tasks, domain.Task{
				Kind: domain.TaskSearch, Source: n, Priority: spec.Priority,
				Payload: domain.TaskPayload{
					Query: spec.Query, Sector: sectorKey, Cell: &c, Language: spec.Language,
					Country: spec.Country, Location: spec.Location, Enrich: spec.Enrich, MaxResults: spec.MaxResults,
					All: spec.IncludeUnrelated,
				},
			})
		}
	}
	e.log.Info("job planned", "job", job.ID, "sources", names, "tasks", len(tasks), "area", area)
	return store.TaskOutcome{Done: true, NewTasks: tasks}
}

func (e *Engine) execSearch(ctx context.Context, t *domain.Task) store.TaskOutcome {
	src, ok := e.sources[t.Source]
	if !ok {
		return store.TaskOutcome{Err: "source not enabled: " + t.Source}
	}
	gov := e.govs[t.Source]
	if err := gov.Wait(ctx); err != nil {
		var open *ratelimit.OpenError
		if errors.As(err, &open) {
			return store.TaskOutcome{RetryAt: open.Until, Err: err.Error(), KeepAttempt: true}
		}
		return store.TaskOutcome{RetryAt: time.Now(), Err: err.Error(), KeepAttempt: true}
	}
	p := t.Payload
	req := source.Request{
		JobID: t.JobID, Query: p.Query, Sector: taxonomy.Get(p.Sector), PageToken: p.PageToken, Page: p.Page,
		Language: p.Language, Country: p.Country, Location: p.Location,
	}
	if p.Cell != nil {
		req.Cell = *p.Cell
	}
	info := src.Info()
	timeout := info.Timeout
	if timeout == 0 {
		timeout = 90 * time.Second
	}
	sctx, cancel := context.WithTimeout(ctx, timeout)
	start := time.Now()
	res, err := src.Search(sctx, req)
	cancel()
	e.m.SourceLatency.WithLabelValues(t.Source).Observe(time.Since(start).Seconds())
	if err != nil {
		return e.sourceError(t, gov, err)
	}
	gov.OnSuccess()
	e.m.SourceRequests.WithLabelValues(t.Source, "ok").Inc()

	out, err := e.persist(ctx, t, res.Businesses, p.Country)
	if err != nil {
		return e.retryOrFail(t, fmt.Errorf("persist: %w", err))
	}

	// follow-ups
	if res.NextPageToken != "" {
		np := p
		np.PageToken, np.Page = res.NextPageToken, p.Page+1
		out.NewTasks = append(out.NewTasks, domain.Task{Kind: domain.TaskSearch, Source: t.Source, Priority: t.Priority, Payload: np})
	}
	if res.Saturated && p.Cell != nil {
		h, w := geo.SizeKm(*p.Cell)
		if min(h, w)/2 >= info.MinCellKm {
			for _, sub := range geo.Split(*p.Cell) {
				sp := p
				sp.Cell, sp.PageToken, sp.Page = &sub, "", 0
				// slightly higher priority: finish refining an area before
				// moving on keeps related duplicates close in time (cache hits).
				out.NewTasks = append(out.NewTasks, domain.Task{Kind: domain.TaskSearch, Source: t.Source, Priority: t.Priority + 1, Payload: sp})
			}
		}
	}
	out.Done = true
	return out
}

func (e *Engine) sourceError(t *domain.Task, gov *ratelimit.Governor, err error) store.TaskOutcome {
	switch {
	case errors.Is(err, source.ErrPermanent):
		e.m.SourceRequests.WithLabelValues(t.Source, "permanent").Inc()
		e.log.Warn("source permanent error", "source", t.Source, "task", t.ID, "err", err)
		return store.TaskOutcome{Err: err.Error()}
	case errors.Is(err, source.ErrBlocked):
		e.m.SourceRequests.WithLabelValues(t.Source, "blocked").Inc()
		until := gov.OnBlocked()
		e.log.Warn("source blocked: circuit open", "source", t.Source, "until", until, "err", err)
		// Provider-side throttling gets a larger, but bounded, retry budget:
		// a provider that never recovers must not keep a job alive forever.
		if t.Attempts >= e.cfg.MaxAttempts*3 {
			return store.TaskOutcome{Err: err.Error()}
		}
		return store.TaskOutcome{RetryAt: until, Err: err.Error()}
	case errors.Is(err, source.ErrRateLimited):
		e.m.SourceRequests.WithLabelValues(t.Source, "rate_limited").Inc()
		gov.OnRateLimited()
		if t.Attempts >= e.cfg.MaxAttempts*3 {
			return store.TaskOutcome{Err: err.Error()}
		}
		after := max(source.RetryAfter(err), e.cfg.MinRetryAfter)
		return store.TaskOutcome{RetryAt: time.Now().Add(after), Err: err.Error()}
	}
	e.m.SourceRequests.WithLabelValues(t.Source, "error").Inc()
	e.log.Warn("source error (will retry)", "source", t.Source, "task", t.ID, "attempt", t.Attempts, "err", err)
	return e.retryOrFail(t, err)
}

// persist normalizes, dedups and stores a page of results, returning the
// counter delta and enrichment follow-ups.
func (e *Engine) persist(ctx context.Context, t *domain.Task, items []domain.Business, country string) (store.TaskOutcome, error) {
	var out store.TaskOutcome
	out.Delta.RawFound = int64(len(items))
	e.m.RecordsRaw.WithLabelValues(t.Source).Add(float64(len(items)))
	if len(items) == 0 {
		return out, nil
	}
	scope := t.JobID + "|" + t.Source
	var stems []string
	if src, ok := e.sources[t.Source]; ok && src.Info().LooseResults && !t.Payload.All {
		texts := []string{t.Payload.Query}
		if sec := taxonomy.Get(t.Payload.Sector); sec != nil {
			texts = append(texts, sec.Label)
			texts = append(texts, sec.Synonyms...)
		}
		stems = normalize.ActivityStems(texts...)
	}
	kept := items[:0]
	keys := make([][]string, 0, len(items))
	for i := range items {
		b := &items[i]
		normalize.Business(b, country)
		if b.Name == "" {
			continue
		}
		if !normalize.MatchesActivity(stems, b.Name, b.Categories) {
			e.m.RecordsUnrelated.WithLabelValues(t.Source).Inc()
			continue
		}
		ks := dedup.Keys(b)
		if len(ks) == 0 {
			continue
		}
		// Same source, same strongest key, same job: an overlapping cell
		// returned it again. Nothing new to merge, skip the database.
		if e.cache.Contains(scope, ks[0]) {
			e.m.DedupCacheHits.Inc()
			continue
		}
		kept = append(kept, *b)
		keys = append(keys, ks)
	}
	if len(kept) == 0 {
		return out, nil
	}
	start := time.Now()
	res, err := e.store.UpsertBusinesses(ctx, t.JobID, kept, keys, t.Payload.MaxResults)
	e.m.UpsertLatency.Observe(time.Since(start).Seconds())
	if err != nil {
		return out, err
	}
	for i := range keys {
		e.cache.Add(scope, keys[i][0])
	}
	for i, r := range res {
		if !r.NewInJob {
			continue
		}
		out.Delta.Unique++
		e.m.RecordsUnique.WithLabelValues(t.Source).Inc()
		if r.Complete {
			out.Delta.Complete++
			e.m.RecordsComplete.WithLabelValues(t.Source).Inc()
		}
		if r.CRMUseful {
			out.Delta.CRMUseful++
			e.m.RecordsUseful.WithLabelValues(t.Source).Inc()
		}
		if _, ok := e.details[t.Source]; ok && r.NeedsDetails {
			// Enrichment, if requested, runs after details (which may reveal
			// the website).
			out.NewTasks = append(out.NewTasks, domain.Task{
				Kind: domain.TaskDetails, Source: DetailsQueue(t.Source), Priority: t.Priority,
				Payload: domain.TaskPayload{BusinessID: r.BusinessID, DataID: r.DataID, Name: kept[i].Name,
					Country: kept[i].Country, Language: t.Payload.Language, Enrich: t.Payload.Enrich},
			})
			continue
		}
		if t.Payload.Enrich && r.NeedsEnrich && e.crawler != nil {
			out.NewTasks = append(out.NewTasks, domain.Task{
				Kind: domain.TaskEnrich, Source: QueueEnrich, Priority: t.Priority - 1,
				Payload: domain.TaskPayload{BusinessID: r.BusinessID, Website: r.Website, Country: kept[i].Country},
			})
		}
	}
	return out, nil
}

func (e *Engine) execEnrich(ctx context.Context, t *domain.Task) store.TaskOutcome {
	p := t.Payload
	res, err := e.crawler.Crawl(ctx, p.Website, p.Country)
	if err != nil {
		e.m.Enriched.WithLabelValues("error").Inc()
		// Mark as attempted so the business is not re-crawled by every job.
		patch := domain.Business{EnrichedAt: time.Now().UTC()}
		_, _, _ = e.store.ApplyPatch(ctx, t.JobID, p.BusinessID, &patch)
		return store.TaskOutcome{Done: true}
	}
	patch := res.Patch
	normalize.Business(&patch, p.Country)
	becameC, becameU, err := e.store.ApplyPatch(ctx, t.JobID, p.BusinessID, &patch)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.TaskOutcome{Done: true}
		}
		return e.retryOrFail(t, err)
	}
	e.m.Enriched.WithLabelValues("ok").Inc()
	out := store.TaskOutcome{Done: true}
	out.Delta.Enriched = 1
	if becameC {
		out.Delta.Complete = 1
		e.m.RecordsComplete.WithLabelValues(QueueEnrich).Inc()
	}
	if becameU {
		out.Delta.CRMUseful = 1
		e.m.RecordsUseful.WithLabelValues(QueueEnrich).Inc()
	}
	return out
}

func (e *Engine) execDetails(ctx context.Context, t *domain.Task) store.TaskOutcome {
	srcName := strings.TrimSuffix(t.Source, ".details")
	d, ok := e.details[srcName]
	if !ok {
		return store.TaskOutcome{Err: "details not available for " + srcName}
	}
	gov := e.govs[srcName]
	if err := gov.Wait(ctx); err != nil {
		var open *ratelimit.OpenError
		if errors.As(err, &open) {
			return store.TaskOutcome{RetryAt: open.Until, Err: err.Error(), KeepAttempt: true}
		}
		return store.TaskOutcome{RetryAt: time.Now(), Err: err.Error(), KeepAttempt: true}
	}
	p := t.Payload
	dctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	start := time.Now()
	patch, err := d.Details(dctx, source.DetailsRequest{DataID: p.DataID, Name: p.Name, Language: p.Language, Country: p.Country})
	cancel()
	e.m.SourceLatency.WithLabelValues(t.Source).Observe(time.Since(start).Seconds())
	if err != nil {
		out := e.sourceError(t, gov, err)
		return out
	}
	gov.OnSuccess()
	e.m.SourceRequests.WithLabelValues(t.Source, "ok").Inc()
	normalize.Business(&patch, p.Country)
	becameC, becameU, err := e.store.ApplyPatch(ctx, t.JobID, p.BusinessID, &patch)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.TaskOutcome{Done: true}
		}
		return e.retryOrFail(t, err)
	}
	out := store.TaskOutcome{Done: true}
	if becameC {
		out.Delta.Complete = 1
		e.m.RecordsComplete.WithLabelValues(t.Source).Inc()
	}
	if becameU {
		out.Delta.CRMUseful = 1
		e.m.RecordsUseful.WithLabelValues(t.Source).Inc()
	}
	if p.Enrich && e.crawler != nil {
		if b, err := e.store.GetBusiness(ctx, p.BusinessID); err == nil && b.Website != "" && b.EnrichedAt.IsZero() {
			out.NewTasks = append(out.NewTasks, domain.Task{
				Kind: domain.TaskEnrich, Source: QueueEnrich, Priority: t.Priority - 1,
				Payload: domain.TaskPayload{BusinessID: p.BusinessID, Website: b.Website, Country: p.Country},
			})
		}
	}
	return out
}
