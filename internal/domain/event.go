package domain

import "time"

// LogEvent is a log record kept for diagnostics. Warnings and errors of every
// process are stored in the database, so any API replica can read what any
// worker reported.
type LogEvent struct {
	ID     int64          `json:"id,omitempty"`
	Time   time.Time      `json:"ts"`
	Level  string         `json:"level"` // DEBUG | INFO | WARN | ERROR
	Msg    string         `json:"msg"`
	JobID  string         `json:"job_id,omitempty"`
	TaskID int64          `json:"task_id,omitempty"`
	Source string         `json:"source,omitempty"` // queue name, e.g. google_maps_http.details
	Worker string         `json:"worker,omitempty"`
	Attrs  map[string]any `json:"attrs,omitempty"`
}

// OutcomeCounts counts source requests by outcome over a time window.
type OutcomeCounts struct {
	OK          int64 `json:"ok"`
	Error       int64 `json:"error"`
	RateLimited int64 `json:"rate_limited"`
	Blocked     int64 `json:"blocked"`
	Permanent   int64 `json:"permanent"`
}

// Total is the number of requests in the window.
func (c OutcomeCounts) Total() int64 {
	return c.OK + c.Error + c.RateLimited + c.Blocked + c.Permanent
}

// Add sums o into c.
func (c *OutcomeCounts) Add(o OutcomeCounts) {
	c.OK += o.OK
	c.Error += o.Error
	c.RateLimited += o.RateLimited
	c.Blocked += o.Blocked
	c.Permanent += o.Permanent
}

// SourceStatus is the live state of one source in one worker. Rate governors
// and circuit breakers are per process, so workers publish them for the API.
type SourceStatus struct {
	Name             string        `json:"name"`
	Rate             float64       `json:"rate"`      // current adaptive rate (req/s)
	BaseRate         float64       `json:"base_rate"` // configured rate (req/s)
	CircuitOpenUntil time.Time     `json:"circuit_open_until,omitzero"`
	InFlight         int64         `json:"in_flight"`
	Recent           OutcomeCounts `json:"recent"` // requests over WindowSeconds
	WindowSeconds    int           `json:"window_seconds"`
}

// WorkerStatus is the heartbeat of an engine process.
type WorkerStatus struct {
	ID        string         `json:"id"`
	Role      string         `json:"role"`
	Version   string         `json:"version,omitempty"`
	StartedAt time.Time      `json:"started_at"`
	SeenAt    time.Time      `json:"seen_at"`
	Sources   []SourceStatus `json:"sources"`
}
