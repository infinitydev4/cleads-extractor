// Package store defines persistence contracts. Two implementations exist:
// pgstore (PostgreSQL, production, horizontally scalable workers) and
// memstore (single process, tests and local development).
package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/cleads/business-extractor/internal/domain"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

// TaskOutcome is applied atomically when a worker finishes a task.
type TaskOutcome struct {
	Done     bool          // success
	Err      string        // last error message
	RetryAt  time.Time     // when non-zero and !Done: put back in queue
	NewTasks []domain.Task // follow-ups (pages, sub-cells, enrichment)
	Delta    domain.Counters
	// KeepAttempt does not count this run as an attempt (rate limiting,
	// circuit breaker): the task did not really fail.
	KeepAttempt bool
}

// UpsertItem is the per-record result of UpsertBusinesses.
type UpsertItem struct {
	BusinessID   int64
	NewInJob     bool // first time this business is attached to the job
	Complete     bool
	CRMUseful    bool
	NeedsEnrich  bool // has a website and was never enriched
	Website      string
	NeedsDetails bool // has a Maps feature id and no place details yet
	DataID       string
}

// BusinessFilter selects records for listings and exports.
type BusinessFilter struct {
	MinCompleteness int
	HasPhone        bool
	HasEmail        bool
	HasWebsite      bool
	OnlyUseful      bool
	City            string
	PostalPrefix    string
	Category        string
	Query           string // name contains (case-insensitive)
}

// BusinessRow is a business plus its position in a job listing.
type BusinessRow struct {
	Seq int64 `json:"seq"`
	domain.Business
}

// Stats is a global snapshot for /v1/stats.
type Stats struct {
	Businesses   int64            `json:"businesses"`
	Jobs         map[string]int64 `json:"jobs"`
	TasksPending map[string]int64 `json:"tasks_pending"` // by source
	TasksRunning int64            `json:"tasks_running"`
}

// Store is the persistence boundary used by the API and the engine.
type Store interface {
	// Jobs
	CreateJob(ctx context.Context, job *domain.Job, tasks []domain.Task) error
	GetJob(ctx context.Context, id string) (domain.Job, error)
	ListJobs(ctx context.Context, status string, limit int, before time.Time) ([]domain.Job, error)
	CancelJob(ctx context.Context, id string) error
	// FinalizeJobs marks running jobs without remaining work as completed
	// and returns them (for webhooks).
	FinalizeJobs(ctx context.Context) ([]domain.Job, error)
	MarkJobRunning(ctx context.Context, id string) error
	FailJob(ctx context.Context, id, reason string) error

	// Queue
	LeaseTasks(ctx context.Context, source string, n int, lease time.Duration) ([]domain.Task, error)
	ExtendLease(ctx context.Context, ids []int64, lease time.Duration) error
	CompleteTask(ctx context.Context, t *domain.Task, out TaskOutcome) error

	// Businesses
	// UpsertBusinesses resolves, merges and links items to the job. When
	// maxInJob > 0, at most that many distinct businesses are ever linked to
	// the job (atomic quota, safe across concurrent workers).
	UpsertBusinesses(ctx context.Context, jobID string, items []domain.Business, keys [][]string, maxInJob int) ([]UpsertItem, error)
	// ApplyPatch merges a partial record (enrichment, place details) into a
	// business and reports whether it crossed the complete / useful bars.
	ApplyPatch(ctx context.Context, jobID string, businessID int64, patch *domain.Business) (becameComplete, becameUseful bool, err error)
	ListJobBusinesses(ctx context.Context, jobID string, afterSeq int64, limit int, f BusinessFilter) ([]BusinessRow, error)
	StreamJobBusinesses(ctx context.Context, jobID string, f BusinessFilter, fn func(*BusinessRow) error) error
	SearchBusinesses(ctx context.Context, afterID int64, limit int, f BusinessFilter) ([]domain.Business, error)
	GetBusiness(ctx context.Context, id int64) (domain.Business, error)

	Stats(ctx context.Context) (Stats, error)
	Ping(ctx context.Context) error
	Close()

	// Diagnostics
	// ListTasks returns a job's remaining tasks (successful tasks are deleted),
	// failed ones first. An empty status means every status.
	ListTasks(ctx context.Context, jobID string, status domain.TaskStatus, limit int) ([]domain.Task, error)
	InsertEvents(ctx context.Context, events []domain.LogEvent) error
	// ListEvents returns matching events, newest first.
	ListEvents(ctx context.Context, f EventFilter) ([]domain.LogEvent, error)
	// PurgeEvents deletes events and worker heartbeats older than before.
	PurgeEvents(ctx context.Context, before time.Time) (int64, error)
	PutWorkerStatus(ctx context.Context, w domain.WorkerStatus) error
	ListWorkerStatus(ctx context.Context, seenAfter time.Time) ([]domain.WorkerStatus, error)
}

// EventFilter selects log events. Zero fields do not filter.
type EventFilter struct {
	Levels   []string // e.g. WARN, ERROR
	JobID    string
	Source   string // exact queue name, or a prefix ending with "*"
	Since    time.Time
	BeforeID int64 // keyset pagination: events with a smaller id
	Limit    int
}

// MatchEvent reports whether e passes f (Limit and BeforeID aside from the
// id bound). Stores that cannot filter natively use it.
func MatchEvent(e *domain.LogEvent, f EventFilter) bool {
	if f.BeforeID > 0 && e.ID >= f.BeforeID {
		return false
	}
	if len(f.Levels) > 0 && !slices.Contains(f.Levels, e.Level) {
		return false
	}
	if f.JobID != "" && e.JobID != f.JobID {
		return false
	}
	if f.Source != "" {
		if p, ok := strings.CutSuffix(f.Source, "*"); ok {
			if !strings.HasPrefix(e.Source, p) {
				return false
			}
		} else if e.Source != f.Source {
			return false
		}
	}
	return f.Since.IsZero() || !e.Time.Before(f.Since)
}
