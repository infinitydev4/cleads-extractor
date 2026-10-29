// Package api is the REST interface. Handlers are thin: they validate input,
// talk to the store and stream output. All heavy work happens in workers, so
// API latency stays in the low milliseconds regardless of extraction load.
package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/time/rate"

	"github.com/cleads/business-extractor/internal/config"
	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/engine"
	"github.com/cleads/business-extractor/internal/export"
	"github.com/cleads/business-extractor/internal/metrics"
	"github.com/cleads/business-extractor/internal/store"
	"github.com/cleads/business-extractor/internal/taxonomy"
)

//go:embed openapi.yaml
var openapiSpec []byte

const adminKeyID = "admin"

type Server struct {
	cfg    *config.Config
	store  store.Store
	eng    *engine.Engine
	m      *metrics.Metrics
	log    *slog.Logger
	limits keyLimiters
	ver    string
}

func New(cfg *config.Config, st store.Store, eng *engine.Engine, m *metrics.Metrics, log *slog.Logger, version string) *Server {
	return &Server{cfg: cfg, store: st, eng: eng, m: m, log: log, ver: version,
		limits: keyLimiters{m: map[string]*rate.Limiter{}, rate: cfg.RatePerKey}}
}

// Handler returns the root handler with middleware applied.
func (s *Server) Handler() http.Handler {
	pub := http.NewServeMux()
	pub.HandleFunc("GET /healthz", s.healthz)
	pub.HandleFunc("GET /readyz", s.readyz)
	pub.HandleFunc("GET /openapi.yaml", s.openapi)
	pub.HandleFunc("GET /docs", s.docs)
	pub.HandleFunc("GET /{$}", s.root)

	priv := http.NewServeMux()
	priv.HandleFunc("GET /v1/sources", s.listSources)
	priv.HandleFunc("GET /v1/sectors", s.listSectors)
	priv.HandleFunc("POST /v1/jobs", s.createJob)
	priv.HandleFunc("GET /v1/jobs", s.listJobs)
	priv.HandleFunc("GET /v1/jobs/{id}", s.getJob)
	priv.HandleFunc("DELETE /v1/jobs/{id}", s.cancelJob)
	priv.HandleFunc("GET /v1/jobs/{id}/businesses", s.jobBusinesses)
	priv.HandleFunc("GET /v1/jobs/{id}/events", s.jobEvents)
	priv.HandleFunc("GET /v1/jobs/{id}/export", s.jobExport)
	priv.HandleFunc("GET /v1/businesses", s.searchBusinesses)
	priv.HandleFunc("GET /v1/businesses/{id}", s.getBusiness)
	priv.HandleFunc("GET /v1/stats", s.stats)
	priv.Handle("GET /metrics", promhttp.HandlerFor(s.m.Registry, promhttp.HandlerOpts{}))

	root := http.NewServeMux()
	root.Handle("/", pub)
	root.Handle("/v1/", s.auth(priv))
	root.Handle("/metrics", s.auth(priv))
	return s.observe(s.security(root))
}

// ---------------------------------------------------------------- helpers

type apiError struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id,omitempty"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, r *http.Request, code int, kind, msg string) {
	var e apiError
	e.Error.Code, e.Error.Message, e.Error.RequestID = kind, msg, requestID(r)
	writeJSON(w, code, e)
}

func (s *Server) storeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", "resource not found")
	case errors.Is(err, store.ErrConflict):
		writeError(w, r, http.StatusConflict, "conflict", "the resource is in a state that does not allow this operation")
	case errors.Is(err, context.Canceled):
		// client went away
	default:
		s.log.Error("store error", "err", err, "request_id", requestID(r))
		writeError(w, r, http.StatusInternalServerError, "internal_error", "internal error")
	}
}

func qInt(r *http.Request, k string, def, lo, hi int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(k))
	if err != nil {
		return def
	}
	return max(lo, min(hi, v))
}

func qBool(r *http.Request, k string) bool {
	v, _ := strconv.ParseBool(r.URL.Query().Get(k))
	return v
}

func filterFrom(r *http.Request) store.BusinessFilter {
	q := r.URL.Query()
	return store.BusinessFilter{
		MinCompleteness: qInt(r, "min_completeness", 0, 0, 100),
		HasPhone:        qBool(r, "has_phone"),
		HasEmail:        qBool(r, "has_email"),
		HasWebsite:      qBool(r, "has_website"),
		OnlyUseful:      qBool(r, "useful"),
		City:            strings.TrimSpace(q.Get("city")),
		PostalPrefix:    strings.TrimSpace(q.Get("postal_code")),
		Category:        strings.TrimSpace(q.Get("category")),
		Query:           strings.TrimSpace(q.Get("q")),
	}
}

// loadJob fetches a job and enforces tenant isolation: a key only sees its
// own jobs, except the "admin" key.
func (s *Server) loadJob(w http.ResponseWriter, r *http.Request) (domain.Job, bool) {
	j, err := s.store.GetJob(r.Context(), r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, err)
		return j, false
	}
	if k := keyID(r); k != "" && k != adminKeyID && j.APIKeyID != k {
		writeError(w, r, http.StatusNotFound, "not_found", "resource not found")
		return j, false
	}
	return j, true
}

func newJobID(apiKey, idem string) string {
	if idem != "" {
		h := sha256.Sum256([]byte(apiKey + "\x00" + idem))
		return "job_" + hex.EncodeToString(h[:12])
	}
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "job_" + hex.EncodeToString(b[:])
}

// ---------------------------------------------------------------- public

func (s *Server) root(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name": "CLEADS Business Extractor API", "version": s.ver, "docs": "/docs", "openapi": "/openapi.yaml",
	})
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.ver})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "reason": "database"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) openapi(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write(openapiSpec)
}

func (s *Server) docs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src https://cdn.jsdelivr.net 'unsafe-inline'; style-src 'unsafe-inline' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; img-src 'self' data: https://cdn.redoc.ly; worker-src blob:")
	_, _ = w.Write([]byte(`<!doctype html><html><head><meta charset="utf-8"><title>CLEADS Business Extractor API</title>
<meta name="viewport" content="width=device-width,initial-scale=1"></head><body>
<redoc spec-url="/openapi.yaml"></redoc>
<script src="https://cdn.jsdelivr.net/npm/redoc@2.1.5/bundles/redoc.standalone.js"></script></body></html>`))
}

// ---------------------------------------------------------------- catalog

func (s *Server) listSources(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sources": s.eng.Sources()})
}

func (s *Server) listSectors(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sectors": taxonomy.All()})
}

// ---------------------------------------------------------------- jobs

type createJobResponse struct {
	Job            domain.Job        `json:"job"`
	PlannedSources []string          `json:"planned_sources"`
	SkippedSources map[string]string `json:"skipped_sources,omitempty"`
	Links          map[string]string `json:"links"`
}

func jobLinks(id string) map[string]string {
	base := "/v1/jobs/" + id
	return map[string]string{
		"self": base, "businesses": base + "/businesses", "events": base + "/events",
		"export_csv": base + "/export?format=csv", "export_jsonl": base + "/export?format=jsonl",
	}
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	var spec domain.JobSpec
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	spec.Normalize()
	if err := spec.Validate(s.cfg.MaxResultsCap); err != nil {
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_spec", err.Error())
		return
	}
	if spec.Sector != "" && taxonomy.Get(spec.Sector) == nil {
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_spec", "unknown sector (see GET /v1/sectors)")
		return
	}
	for _, n := range spec.Sources {
		if !s.eng.HasSource(n) {
			writeError(w, r, http.StatusUnprocessableEntity, "invalid_spec", fmt.Sprintf("source %q is not enabled (see GET /v1/sources)", n))
			return
		}
	}
	planned, skipped := s.eng.SelectSources(&spec)
	if len(planned) == 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": map[string]string{"code": "no_source", "message": "no enabled source can serve this request"}, "skipped_sources": skipped})
		return
	}
	job := domain.Job{
		ID: newJobID(keyID(r), r.Header.Get("Idempotency-Key")), Spec: spec, Status: domain.JobQueued,
		CreatedAt: time.Now().UTC(), APIKeyID: keyID(r),
	}
	err := s.store.CreateJob(r.Context(), &job, []domain.Task{engine.PlanTask(&job)})
	if errors.Is(err, store.ErrConflict) && r.Header.Get("Idempotency-Key") != "" {
		existing, gerr := s.store.GetJob(r.Context(), job.ID)
		if gerr != nil {
			s.storeError(w, r, gerr)
			return
		}
		writeJSON(w, http.StatusOK, createJobResponse{Job: existing, PlannedSources: planned, SkippedSources: skipped, Links: jobLinks(existing.ID)})
		return
	}
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/jobs/"+job.ID)
	writeJSON(w, http.StatusAccepted, createJobResponse{Job: job, PlannedSources: planned, SkippedSources: skipped, Links: jobLinks(job.ID)})
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	var before time.Time
	if v := r.URL.Query().Get("before"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "invalid_param", "before must be RFC3339")
			return
		}
		before = t
	}
	limit := qInt(r, "limit", 50, 1, 200)
	jobs, err := s.store.ListJobs(r.Context(), r.URL.Query().Get("status"), limit*4, before)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	k := keyID(r)
	out := make([]domain.Job, 0, limit)
	for _, j := range jobs {
		if k == "" || k == adminKeyID || j.APIKeyID == k {
			out = append(out, j)
		}
		if len(out) == limit {
			break
		}
	}
	resp := map[string]any{"jobs": out}
	if len(out) == limit {
		resp["next_before"] = out[len(out)-1].CreatedAt.Format(time.RFC3339Nano)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	j, ok := s.loadJob(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": j, "links": jobLinks(j.ID)})
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	j, ok := s.loadJob(w, r)
	if !ok {
		return
	}
	if err := s.store.CancelJob(r.Context(), j.ID); err != nil {
		s.storeError(w, r, err)
		return
	}
	j, _ = s.store.GetJob(r.Context(), j.ID)
	writeJSON(w, http.StatusOK, map[string]any{"job": j})
}

// jobBusinesses returns results progressively with a stable cursor: clients
// poll with ?cursor=<next_cursor> and receive only new records.
func (s *Server) jobBusinesses(w http.ResponseWriter, r *http.Request) {
	j, ok := s.loadJob(w, r)
	if !ok {
		return
	}
	cursor, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
	limit := qInt(r, "limit", 100, 1, 1000)
	rows, err := s.store.ListJobBusinesses(r.Context(), j.ID, cursor, limit, filterFrom(r))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	next := cursor
	if len(rows) > 0 {
		next = rows[len(rows)-1].Seq
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": rows, "next_cursor": strconv.FormatInt(next, 10), "has_more": len(rows) == limit,
		"job_status": j.Status, "counters": j.Counters,
	})
}

// jobEvents streams progress as Server-Sent Events until the job ends.
func (s *Server) jobEvents(w http.ResponseWriter, r *http.Request) {
	j, ok := s.loadJob(w, r)
	if !ok {
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	interval := time.Duration(qInt(r, "interval_ms", 1000, 250, 10_000)) * time.Millisecond
	t := time.NewTicker(interval)
	defer t.Stop()
	var last domain.Counters
	lastStatus := domain.JobStatus("")
	idle := 0
	for {
		if j.Counters != last || j.Status != lastStatus {
			b, _ := json.Marshal(map[string]any{"id": j.ID, "status": j.Status, "counters": j.Counters, "error": j.Error})
			if _, err := fmt.Fprintf(w, "event: progress\ndata: %s\n\n", b); err != nil {
				return
			}
			last, lastStatus, idle = j.Counters, j.Status, 0
		} else if idle++; idle%15 == 0 {
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
		}
		if err := rc.Flush(); err != nil {
			return
		}
		if j.Status.Terminal() {
			fmt.Fprintf(w, "event: done\ndata: {\"status\":%q}\n\n", j.Status)
			_ = rc.Flush()
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
		}
		var err error
		if j, err = s.store.GetJob(r.Context(), j.ID); err != nil {
			return
		}
	}
}

func (s *Server) jobExport(w http.ResponseWriter, r *http.Request) {
	j, ok := s.loadJob(w, r)
	if !ok {
		return
	}
	format := r.URL.Query().Get("format")
	ew, err := export.New(w, format)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_param", err.Error())
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", ew.ContentType())
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="cleads-%s.%s"`, j.ID, ew.Ext()))
	w.Header().Set("X-Job-Status", string(j.Status))
	n := 0
	err = s.store.StreamJobBusinesses(r.Context(), j.ID, filterFrom(r), func(row *store.BusinessRow) error {
		n++
		b := row.Business
		if err := ew.Write(&b); err != nil {
			return err
		}
		if n%2000 == 0 {
			if f, ok := ew.(export.Flusher); ok {
				_ = f.Flush()
			}
			_ = rc.Flush()
		}
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		s.log.Error("export failed", "job", j.ID, "rows", n, "err", err)
	}
	_ = ew.Close()
}

// ---------------------------------------------------------------- businesses

func (s *Server) searchBusinesses(w http.ResponseWriter, r *http.Request) {
	cursor, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
	limit := qInt(r, "limit", 100, 1, 1000)
	items, err := s.store.SearchBusinesses(r.Context(), cursor, limit, filterFrom(r))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	next := cursor
	if len(items) > 0 {
		next = items[len(items)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": strconv.FormatInt(next, 10), "has_more": len(items) == limit})
}

func (s *Server) getBusiness(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_param", "id must be an integer")
		return
	}
	b, err := s.store.GetBusiness(r.Context(), id)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.Stats(r.Context())
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}
