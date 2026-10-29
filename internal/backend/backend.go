// Package backend is the contract between the MCP tools and the extractor.
// It is implemented in-process by the API server (direct store and engine
// calls, used by /mcp) and over HTTP by apiclient (used by the stdio MCP
// command, which talks to a remote instance). The REST API returns exactly
// these types, so both implementations behave the same.
package backend

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/source"
	"github.com/cleads/business-extractor/internal/store"
	"github.com/cleads/business-extractor/internal/taxonomy"
)

// Backend is everything an agent can do. Every call acts as one API key:
// jobs of other keys are invisible (not found), except for the admin key.
type Backend interface {
	Sources(ctx context.Context) ([]source.Info, error)
	Sectors(ctx context.Context) ([]taxonomy.Sector, error)
	OpenAPI(ctx context.Context) ([]byte, error)

	// CreateJob submits spec. A non-empty idempotencyKey makes retries
	// return the existing job (Replayed) instead of creating another one.
	CreateJob(ctx context.Context, spec domain.JobSpec, idempotencyKey string) (CreateJobResult, error)
	GetJob(ctx context.Context, id string) (domain.Job, error)
	ListJobs(ctx context.Context, status string, limit int, before time.Time) (JobList, error)
	CancelJob(ctx context.Context, id string) (domain.Job, error)
	JobBusinesses(ctx context.Context, id string, cursor int64, limit int, f store.BusinessFilter) (JobBusinessPage, error)
	// ExportChunk renders up to limit rows (and at most maxBytes) of a job
	// export after cursor. The CSV header is included when cursor is 0.
	ExportChunk(ctx context.Context, id, format string, f store.BusinessFilter, cursor int64, limit, maxBytes int) (ExportChunk, error)

	SearchBusinesses(ctx context.Context, cursor int64, limit int, f store.BusinessFilter) (BusinessPage, error)
	GetBusiness(ctx context.Context, id int64) (domain.Business, error)

	JobTasks(ctx context.Context, id string, status domain.TaskStatus, limit int) (TaskList, error)
	JobErrors(ctx context.Context, id string) (JobErrors, error)
	// Logs requires the admin key (logs mix every client).
	Logs(ctx context.Context, q LogQuery) (LogList, error)
	Health(ctx context.Context) (HealthSummary, error)
	Stats(ctx context.Context) (store.Stats, error)
}

// Error is an API error with its HTTP status and stable code.
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// Errorf builds an *Error.
func Errorf(status int, code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Code returns the API error code of err, or "".
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

type CreateJobResult struct {
	Job            domain.Job        `json:"job"`
	PlannedSources []string          `json:"planned_sources"`
	SkippedSources map[string]string `json:"skipped_sources,omitempty"`
	Links          map[string]string `json:"links"`
	// Replayed is true when the idempotency key matched an existing job.
	Replayed bool `json:"replayed,omitempty"`
}

type JobList struct {
	Jobs       []domain.Job `json:"jobs"`
	NextBefore string       `json:"next_before,omitempty"`
}

type JobBusinessPage struct {
	Items      []store.BusinessRow `json:"items"`
	NextCursor string              `json:"next_cursor"`
	HasMore    bool                `json:"has_more"`
	JobStatus  domain.JobStatus    `json:"job_status"`
	Counters   domain.Counters     `json:"counters"`
}

type BusinessPage struct {
	Items      []domain.Business `json:"items"`
	NextCursor string            `json:"next_cursor"`
	HasMore    bool              `json:"has_more"`
}

type ExportChunk struct {
	Format      string `json:"format"`
	ContentType string `json:"content_type"`
	Text        string `json:"text"`
	Rows        int    `json:"rows"`
	NextCursor  string `json:"next_cursor"`
	Done        bool   `json:"done"`
}

type TaskList struct {
	Tasks []domain.Task `json:"tasks"`
}

// ErrorGroup is a set of failures sharing the same cause.
type ErrorGroup struct {
	Count    int       `json:"count"`
	Kind     string    `json:"kind"` // failed_task | retrying_task | event
	Source   string    `json:"source,omitempty"`
	Cause    string    `json:"cause"`   // normalized message
	Example  string    `json:"example"` // one full message (redacted)
	LastSeen time.Time `json:"last_seen,omitzero"`
	TaskIDs  []int64   `json:"task_ids,omitempty"` // a few examples
}

type JobErrors struct {
	JobID         string           `json:"job_id"`
	Status        domain.JobStatus `json:"status"`
	JobError      string           `json:"job_error,omitempty"`
	TasksFailed   int64            `json:"tasks_failed"`
	TasksRetrying int              `json:"tasks_retrying"`
	Groups        []ErrorGroup     `json:"groups"`
	Summary       string           `json:"summary"`
}

type LogQuery struct {
	Level  string // minimum level: DEBUG, INFO, WARN (default), ERROR
	JobID  string
	Source string // exact queue name, or a prefix ending with "*"
	Since  time.Time
	Limit  int
	// Local reads the in-memory ring of the serving process (INFO and
	// above, recent only) instead of the shared events table.
	Local bool
}

type LogList struct {
	Scope  string            `json:"scope"` // database | process
	Events []domain.LogEvent `json:"events"`
}

type HealthSummary struct {
	Status    string    `json:"status"`  // ok | degraded | down
	Summary   string    `json:"summary"` // one line per finding
	CheckedAt time.Time `json:"checked_at"`
	Database  struct {
		OK        bool    `json:"ok"`
		LatencyMS float64 `json:"latency_ms"`
		Error     string  `json:"error,omitempty"`
	} `json:"database"`
	Queue struct {
		Pending map[string]int64 `json:"pending"`
		Running int64            `json:"running"`
	} `json:"queue"`
	Jobs    map[string]int64 `json:"jobs"`
	Workers []WorkerBrief    `json:"workers"`
	Sources []SourceHealth   `json:"sources"`
	// RecentErrors are the last warnings and errors (admin key only: they
	// mix every client). RecentErrorCount is given to every key.
	RecentErrors     []domain.LogEvent `json:"recent_errors,omitempty"`
	RecentErrorCount int               `json:"recent_error_count"`
	RecentWindow     string            `json:"recent_window"`
}

type WorkerBrief struct {
	ID      string    `json:"id"`
	Role    string    `json:"role"`
	Version string    `json:"version,omitempty"`
	SeenAt  time.Time `json:"seen_at"`
}

// SourceHealth aggregates one source over the live workers.
type SourceHealth struct {
	Name string `json:"name"`
	// Verdict: ok | idle | throttled | blocked | failing | no_worker.
	Verdict          string               `json:"verdict"`
	Detail           string               `json:"detail"`
	CircuitOpen      bool                 `json:"circuit_open"`
	CircuitOpenUntil time.Time            `json:"circuit_open_until,omitzero"`
	Rate             float64              `json:"rate"`      // sum over workers (req/s)
	BaseRate         float64              `json:"base_rate"` // sum over workers (req/s)
	Workers          int                  `json:"workers"`
	InFlight         int64                `json:"in_flight"`
	Pending          int64                `json:"pending"` // search + details queues
	Recent           domain.OutcomeCounts `json:"recent"`
	ErrorRate        float64              `json:"error_rate"` // non-ok share of recent requests
	WindowSeconds    int                  `json:"window_seconds"`
}
