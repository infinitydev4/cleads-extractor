package memstore

import (
	"context"
	"testing"
	"time"

	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/store"
)

func TestTerminalJobsAreNotifiedExactlyOnce(t *testing.T) {
	ctx := context.Background()
	s := New()
	for _, id := range []string{"done", "cancel"} {
		j := domain.Job{ID: id, Status: domain.JobQueued, CreatedAt: time.Now()}
		if err := s.CreateJob(ctx, &j, []domain.Task{{Kind: domain.TaskSearch, Source: "x"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CancelJob(ctx, "cancel"); err != nil {
		t.Fatal(err)
	}
	tasks, _ := s.LeaseTasks(ctx, "x", 10, time.Minute)
	if len(tasks) != 1 || tasks[0].JobID != "done" {
		t.Fatalf("cancelled job tasks must be dropped, got %+v", tasks)
	}
	if err := s.CompleteTask(ctx, &tasks[0], store.TaskOutcome{Done: true, Delta: domain.Counters{Unique: 1}}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.FinalizeJobs(ctx)
	if len(got) != 2 {
		t.Fatalf("want both terminal jobs reported, got %d", len(got))
	}
	if again, _ := s.FinalizeJobs(ctx); len(again) != 0 {
		t.Fatalf("jobs reported twice: %+v", again)
	}
}

func TestStaleCompletionIsFenced(t *testing.T) {
	ctx := context.Background()
	s := New()
	j := domain.Job{ID: "j", Status: domain.JobQueued, CreatedAt: time.Now()}
	_ = s.CreateJob(ctx, &j, []domain.Task{{Kind: domain.TaskSearch, Source: "x"}})
	a, _ := s.LeaseTasks(ctx, "x", 1, 10*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	b, _ := s.LeaseTasks(ctx, "x", 1, time.Minute)
	if len(b) != 1 || b[0].Attempts != 2 {
		t.Fatalf("expired lease not reclaimed: %+v", b)
	}
	if err := s.CompleteTask(ctx, &a[0], store.TaskOutcome{Done: true}); err != store.ErrConflict {
		t.Fatalf("stale completion accepted: %v", err)
	}
	if err := s.CompleteTask(ctx, &b[0], store.TaskOutcome{Done: true}); err != nil {
		t.Fatal(err)
	}
}
