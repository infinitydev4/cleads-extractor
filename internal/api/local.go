package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cleads/business-extractor/internal/backend"
	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/engine"
	"github.com/cleads/business-extractor/internal/export"
	"github.com/cleads/business-extractor/internal/source"
	"github.com/cleads/business-extractor/internal/store"
	"github.com/cleads/business-extractor/internal/taxonomy"
)

// Backend returns the in-process backend acting as API key keyID ("" when
// auth is disabled: everything is visible). REST handlers and the /mcp
// endpoint both go through it, so they enforce the same rules.
func (s *Server) Backend(keyID string) backend.Backend { return &local{s: s, key: keyID} }

type local struct {
	s   *Server
	key string
}

var (
	errNotFound  = backend.Errorf(http.StatusNotFound, "not_found", "resource not found")
	errConflict  = backend.Errorf(http.StatusConflict, "conflict", "the resource is in a state that does not allow this operation")
	errInternal  = backend.Errorf(http.StatusInternalServerError, "internal_error", "internal error")
	errAdminOnly = backend.Errorf(http.StatusForbidden, "forbidden", "logs mix every client: only the admin API key may read them")
)

func (l *local) admin() bool { return l.key == "" || l.key == adminKeyID }

// fail maps store errors to API errors; unexpected errors are logged here
// and never returned verbatim (they could carry internal details).
func (l *local) fail(ctx context.Context, op string, err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return errNotFound
	case errors.Is(err, store.ErrConflict):
		return errConflict
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	var be *backend.Error
	if errors.As(err, &be) {
		return be
	}
	l.s.log.ErrorContext(ctx, "store error", "op", op, "err", err)
	return errInternal
}

// job fetches a job and enforces tenant isolation: a key only sees its own
// jobs, except the admin key. Other tenants' jobs are reported as missing.
func (l *local) job(ctx context.Context, id string) (domain.Job, error) {
	j, err := l.s.store.GetJob(ctx, id)
	if err != nil {
		return j, l.fail(ctx, "get job", err)
	}
	if !l.admin() && j.APIKeyID != l.key {
		return domain.Job{}, errNotFound
	}
	return j, nil
}

func (l *local) Sources(context.Context) ([]source.Info, error) {
	out := l.s.eng.Sources()
	slices.SortFunc(out, func(a, b source.Info) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (l *local) Sectors(context.Context) ([]taxonomy.Sector, error) { return taxonomy.All(), nil }

func (l *local) OpenAPI(context.Context) ([]byte, error) { return openapiSpec, nil }

func (l *local) CreateJob(ctx context.Context, spec domain.JobSpec, idem string) (backend.CreateJobResult, error) {
	var res backend.CreateJobResult
	if len(idem) > 200 {
		return res, backend.Errorf(http.StatusBadRequest, "invalid_param", "Idempotency-Key is too long (max 200)")
	}
	spec.Normalize()
	if err := spec.Validate(l.s.cfg.MaxResultsCap); err != nil {
		return res, backend.Errorf(http.StatusUnprocessableEntity, "invalid_spec", "%s", err.Error())
	}
	if spec.Sector != "" && taxonomy.Get(spec.Sector) == nil {
		return res, backend.Errorf(http.StatusUnprocessableEntity, "invalid_spec", "unknown sector (see GET /v1/sectors)")
	}
	for _, n := range spec.Sources {
		if !l.s.eng.HasSource(n) {
			return res, backend.Errorf(http.StatusUnprocessableEntity, "invalid_spec", "source %q is not enabled (see GET /v1/sources)", n)
		}
	}
	planned, skipped := l.s.eng.SelectSources(&spec)
	if len(planned) == 0 {
		return res, &noSourceError{skipped: skipped}
	}
	job := domain.Job{
		ID: newJobID(l.key, idem), Spec: spec, Status: domain.JobQueued, CreatedAt: time.Now().UTC(), APIKeyID: l.key,
	}
	err := l.s.store.CreateJob(ctx, &job, []domain.Task{engine.PlanTask(&job)})
	if errors.Is(err, store.ErrConflict) && idem != "" {
		existing, gerr := l.s.store.GetJob(ctx, job.ID)
		if gerr != nil {
			return res, l.fail(ctx, "get job", gerr)
		}
		return backend.CreateJobResult{Job: existing, PlannedSources: planned, SkippedSources: skipped,
			Links: jobLinks(existing.ID), Replayed: true}, nil
	}
	if err != nil {
		return res, l.fail(ctx, "create job", err)
	}
	return backend.CreateJobResult{Job: job, PlannedSources: planned, SkippedSources: skipped, Links: jobLinks(job.ID)}, nil
}

// noSourceError keeps the per-source reasons for the REST error body.
type noSourceError struct{ skipped map[string]string }

func (e *noSourceError) Error() string {
	return "no_source: no enabled source can serve this request " + fmt.Sprint(e.skipped)
}

func (l *local) GetJob(ctx context.Context, id string) (domain.Job, error) { return l.job(ctx, id) }

func (l *local) ListJobs(ctx context.Context, status string, limit int, before time.Time) (backend.JobList, error) {
	limit = max(1, min(200, limit))
	// Over-fetch: other tenants' jobs are filtered out below.
	jobs, err := l.s.store.ListJobs(ctx, status, limit*4, before)
	if err != nil {
		return backend.JobList{}, l.fail(ctx, "list jobs", err)
	}
	out := backend.JobList{Jobs: make([]domain.Job, 0, limit)}
	for _, j := range jobs {
		if l.admin() || j.APIKeyID == l.key {
			out.Jobs = append(out.Jobs, j)
		}
		if len(out.Jobs) == limit {
			break
		}
	}
	if len(out.Jobs) == limit {
		out.NextBefore = out.Jobs[len(out.Jobs)-1].CreatedAt.Format(time.RFC3339Nano)
	}
	return out, nil
}

func (l *local) CancelJob(ctx context.Context, id string) (domain.Job, error) {
	j, err := l.job(ctx, id)
	if err != nil {
		return j, err
	}
	if err := l.s.store.CancelJob(ctx, j.ID); err != nil {
		return j, l.fail(ctx, "cancel job", err)
	}
	j, err = l.s.store.GetJob(ctx, j.ID)
	if err != nil {
		return j, l.fail(ctx, "get job", err)
	}
	return j, nil
}

func (l *local) JobBusinesses(ctx context.Context, id string, cursor int64, limit int, f store.BusinessFilter) (backend.JobBusinessPage, error) {
	var page backend.JobBusinessPage
	j, err := l.job(ctx, id)
	if err != nil {
		return page, err
	}
	limit = max(1, min(1000, limit))
	rows, err := l.s.store.ListJobBusinesses(ctx, j.ID, cursor, limit, f)
	if err != nil {
		return page, l.fail(ctx, "list job businesses", err)
	}
	next := cursor
	if len(rows) > 0 {
		next = rows[len(rows)-1].Seq
	}
	if rows == nil {
		rows = []store.BusinessRow{}
	}
	return backend.JobBusinessPage{Items: rows, NextCursor: strconv.FormatInt(next, 10), HasMore: len(rows) == limit,
		JobStatus: j.Status, Counters: j.Counters}, nil
}

func (l *local) ExportChunk(ctx context.Context, id, format string, f store.BusinessFilter, cursor int64, limit, maxBytes int) (backend.ExportChunk, error) {
	var out backend.ExportChunk
	j, err := l.job(ctx, id)
	if err != nil {
		return out, err
	}
	limit = max(1, min(1000, limit))
	if maxBytes <= 0 || maxBytes > 1<<20 {
		maxBytes = 1 << 20
	}
	var buf bytes.Buffer
	newW := export.NewContinuation
	if cursor == 0 {
		newW = export.New
	}
	ew, err := newW(&buf, format)
	if err != nil {
		return out, backend.Errorf(http.StatusBadRequest, "invalid_param", "%s", err.Error())
	}
	rows, err := l.s.store.ListJobBusinesses(ctx, j.ID, cursor, limit, f)
	if err != nil {
		return out, l.fail(ctx, "list job businesses", err)
	}
	flush := func() { _ = ew.(export.Flusher).Flush() }
	next := cursor
	for i := range rows {
		mark := buf.Len()
		if err := ew.Write(&rows[i].Business); err != nil {
			return out, l.fail(ctx, "export", err)
		}
		flush()
		if buf.Len() > maxBytes && out.Rows > 0 {
			buf.Truncate(mark) // keep whole rows only
			break
		}
		out.Rows++
		next = rows[i].Seq
	}
	if out.Rows == 0 && cursor == 0 {
		_ = ew.Close() // header only
	}
	out.Format, out.ContentType, out.Text = format, ew.ContentType(), buf.String()
	if out.Format == "" {
		out.Format = "csv"
	}
	out.NextCursor = strconv.FormatInt(next, 10)
	out.Done = out.Rows == len(rows) && len(rows) < limit
	return out, nil
}

func (l *local) SearchBusinesses(ctx context.Context, cursor int64, limit int, f store.BusinessFilter) (backend.BusinessPage, error) {
	limit = max(1, min(1000, limit))
	items, err := l.s.store.SearchBusinesses(ctx, cursor, limit, f)
	if err != nil {
		return backend.BusinessPage{}, l.fail(ctx, "search businesses", err)
	}
	next := cursor
	if len(items) > 0 {
		next = items[len(items)-1].ID
	}
	if items == nil {
		items = []domain.Business{}
	}
	return backend.BusinessPage{Items: items, NextCursor: strconv.FormatInt(next, 10), HasMore: len(items) == limit}, nil
}

func (l *local) GetBusiness(ctx context.Context, id int64) (domain.Business, error) {
	b, err := l.s.store.GetBusiness(ctx, id)
	if err != nil {
		return b, l.fail(ctx, "get business", err)
	}
	return b, nil
}

func (l *local) JobTasks(ctx context.Context, id string, status domain.TaskStatus, limit int) (backend.TaskList, error) {
	switch status {
	case "", domain.TaskFailed, domain.TaskPending, domain.TaskRunning:
	default:
		return backend.TaskList{}, backend.Errorf(http.StatusBadRequest, "invalid_param", "status must be failed, pending or running")
	}
	j, err := l.job(ctx, id)
	if err != nil {
		return backend.TaskList{}, err
	}
	ts, err := l.s.store.ListTasks(ctx, j.ID, status, max(1, min(500, limit)))
	if err != nil {
		return backend.TaskList{}, l.fail(ctx, "list tasks", err)
	}
	for i := range ts {
		ts[i].LastError = l.s.redact.String(ts[i].LastError)
	}
	if ts == nil {
		ts = []domain.Task{}
	}
	return backend.TaskList{Tasks: ts}, nil
}

func (l *local) JobErrors(ctx context.Context, id string) (backend.JobErrors, error) {
	j, err := l.job(ctx, id)
	if err != nil {
		return backend.JobErrors{}, err
	}
	failed, err := l.s.store.ListTasks(ctx, j.ID, domain.TaskFailed, 1000)
	if err != nil {
		return backend.JobErrors{}, l.fail(ctx, "list tasks", err)
	}
	pending, err := l.s.store.ListTasks(ctx, j.ID, domain.TaskPending, 1000)
	if err != nil {
		return backend.JobErrors{}, l.fail(ctx, "list tasks", err)
	}
	events, err := l.s.store.ListEvents(ctx, store.EventFilter{JobID: j.ID, Levels: []string{"WARN", "ERROR"}, Limit: 500})
	if err != nil {
		return backend.JobErrors{}, l.fail(ctx, "list events", err)
	}
	return groupJobErrors(j, failed, pending, events, l.s.redact), nil
}

func (l *local) Logs(ctx context.Context, q backend.LogQuery) (backend.LogList, error) {
	if !l.admin() {
		return backend.LogList{}, errAdminOnly
	}
	lvls, minLevel, err := levelsFrom(q.Level)
	if err != nil {
		return backend.LogList{}, err
	}
	limit := max(1, min(500, q.Limit))
	if q.Local {
		var evs []domain.LogEvent
		if l.s.ring != nil {
			evs = l.s.ring.Snapshot(minLevel, q.JobID, trimStar(q.Source), limit)
		}
		if !q.Since.IsZero() {
			evs = slices.DeleteFunc(evs, func(e domain.LogEvent) bool { return e.Time.Before(q.Since) })
		}
		return backend.LogList{Scope: "process", Events: nonNil(evs)}, nil
	}
	evs, err := l.s.store.ListEvents(ctx, store.EventFilter{Levels: lvls, JobID: q.JobID, Source: q.Source, Since: q.Since, Limit: limit})
	if err != nil {
		return backend.LogList{}, l.fail(ctx, "list events", err)
	}
	return backend.LogList{Scope: "database", Events: nonNil(evs)}, nil
}

func (l *local) Stats(ctx context.Context) (store.Stats, error) {
	st, err := l.s.store.Stats(ctx)
	if err != nil {
		return st, l.fail(ctx, "stats", err)
	}
	return st, nil
}

func nonNil(evs []domain.LogEvent) []domain.LogEvent {
	if evs == nil {
		return []domain.LogEvent{}
	}
	return evs
}
