package pgstore

import (
	"context"
	"testing"
	"time"

	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/store"
)

func TestMigration002(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version IN (1, 2)`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("migrations applied: %d %v", n, err)
	}
	for _, idx := range []string{"events_job_ts_idx", "events_ts_idx"} {
		var ok bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = $1)`, idx).Scan(&ok); err != nil || !ok {
			t.Fatalf("index %s missing (%v)", idx, err)
		}
	}
	// Re-running migrations is a no-op (replicas start concurrently).
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestEvents(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	evs := []domain.LogEvent{
		{Time: now.Add(-2 * time.Hour), Level: "WARN", Msg: "old", JobID: "j1", Source: "google_maps_http"},
		{Time: now.Add(-time.Minute), Level: "WARN", Msg: "source error (will retry)", JobID: "j1", TaskID: 7,
			Source: "google_maps_http.details", Worker: "w1", Attrs: map[string]any{"err": "http 500", "attempt": 2}},
		{Time: now, Level: "ERROR", Msg: "boom", JobID: "j2", Source: "osm"},
		{Time: now, Level: "INFO", Msg: "info"},
	}
	if err := s.InsertEvents(ctx, evs); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListEvents(ctx, store.EventFilter{Levels: []string{"WARN", "ERROR"}, Source: "google_maps_http*", Since: now.Add(-time.Hour)})
	if err != nil || len(got) != 1 {
		t.Fatalf("filtered: %v %+v", err, got)
	}
	e := got[0]
	if e.TaskID != 7 || e.Worker != "w1" || e.Attrs["err"] != "http 500" || e.Attrs["attempt"] != float64(2) || !e.Time.Equal(evs[1].Time) {
		t.Fatalf("round trip: %+v", e)
	}
	all, _ := s.ListEvents(ctx, store.EventFilter{Limit: 2})
	if len(all) != 2 || all[0].ID < all[1].ID {
		t.Fatalf("newest first: %+v", all)
	}
	older, _ := s.ListEvents(ctx, store.EventFilter{BeforeID: all[1].ID})
	if len(older) != 2 {
		t.Fatalf("keyset page: %d", len(older))
	}
	if got, _ := s.ListEvents(ctx, store.EventFilter{JobID: "j2", Levels: []string{"ERROR"}}); len(got) != 1 || got[0].Msg != "boom" {
		t.Fatalf("by job: %+v", got)
	}
	n, err := s.PurgeEvents(ctx, now.Add(-time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
}

func TestWorkerStatusAndTasks(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now().UTC()
	w := domain.WorkerStatus{ID: "w1", Role: "worker", StartedAt: now, SeenAt: now,
		Sources: []domain.SourceStatus{{Name: "osm", Rate: 1, BaseRate: 2, Recent: domain.OutcomeCounts{OK: 3, Blocked: 1}}}}
	if err := s.PutWorkerStatus(ctx, w); err != nil {
		t.Fatal(err)
	}
	w.SeenAt = now.Add(time.Second)
	w.Sources[0].Rate = 2
	if err := s.PutWorkerStatus(ctx, w); err != nil { // upsert
		t.Fatal(err)
	}
	_ = s.PutWorkerStatus(ctx, domain.WorkerStatus{ID: "gone", Role: "worker", StartedAt: now, SeenAt: now.Add(-time.Hour)})
	ws, err := s.ListWorkerStatus(ctx, now.Add(-time.Minute))
	if err != nil || len(ws) != 1 || ws[0].Sources[0].Rate != 2 || ws[0].Sources[0].Recent.Blocked != 1 {
		t.Fatalf("workers: %v %+v", err, ws)
	}

	j := domain.Job{ID: "job_diag", Spec: domain.JobSpec{Query: "x", Location: "Lyon"}, Status: domain.JobRunning, CreatedAt: now}
	if err := s.CreateJob(ctx, &j, []domain.Task{{Kind: domain.TaskPlan, Source: "_plan"}}); err != nil {
		t.Fatal(err)
	}
	leased, err := s.LeaseTasks(ctx, "_plan", 10, time.Minute)
	if err != nil || len(leased) != 1 {
		t.Fatalf("lease: %v %d", err, len(leased))
	}
	if err := s.CompleteTask(ctx, &leased[0], store.TaskOutcome{Err: "geocode: boom", NewTasks: []domain.Task{
		{Kind: domain.TaskSearch, Source: "osm"}}}); err != nil {
		t.Fatal(err)
	}
	ts, err := s.ListTasks(ctx, j.ID, "", 10)
	if err != nil || len(ts) != 2 || ts[0].Status != domain.TaskFailed || ts[0].LastError != "geocode: boom" || ts[1].Status != domain.TaskPending {
		t.Fatalf("tasks: %v %+v", err, ts)
	}
	if ts, _ := s.ListTasks(ctx, j.ID, domain.TaskPending, 10); len(ts) != 1 || ts[0].Source != "osm" {
		t.Fatalf("pending: %+v", ts)
	}
}
