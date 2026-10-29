package domain

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// Point is a WGS84 coordinate.
type Point struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// BBox is a WGS84 bounding box.
type BBox struct {
	MinLat float64 `json:"min_lat"`
	MinLng float64 `json:"min_lng"`
	MaxLat float64 `json:"max_lat"`
	MaxLng float64 `json:"max_lng"`
}

// Valid reports whether the box is a finite, non-empty rectangle.
func (b BBox) Valid() bool {
	for _, v := range []float64{b.MinLat, b.MinLng, b.MaxLat, b.MaxLng} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return b.MinLat < b.MaxLat && b.MinLng < b.MaxLng &&
		b.MinLat >= -90 && b.MaxLat <= 90 && b.MinLng >= -180 && b.MaxLng <= 180
}

// Center returns the middle of the box.
func (b BBox) Center() Point {
	return Point{Lat: (b.MinLat + b.MaxLat) / 2, Lng: (b.MinLng + b.MaxLng) / 2}
}

// Contains reports whether p is inside the box (inclusive).
func (b BBox) Contains(p Point) bool {
	return p.Lat >= b.MinLat && p.Lat <= b.MaxLat && p.Lng >= b.MinLng && p.Lng <= b.MaxLng
}

type JobStatus string

const (
	JobQueued    JobStatus = "queued"
	JobRunning   JobStatus = "running"
	JobCompleted JobStatus = "completed"
	JobFailed    JobStatus = "failed"
	JobCancelled JobStatus = "cancelled"
)

// Terminal reports whether the job will not change state anymore.
func (s JobStatus) Terminal() bool {
	return s == JobCompleted || s == JobFailed || s == JobCancelled
}

// JobSpec is what a client submits.
type JobSpec struct {
	// Query is the free-text activity, e.g. "plombier" or "agence immobilière".
	Query string `json:"query"`
	// Sector is an optional taxonomy key (see GET /v1/sectors) that maps the
	// activity to each source's native classification.
	Sector string `json:"sector,omitempty"`

	// One of Location (geocoded), BBox or Center+RadiusM is required.
	Location string  `json:"location,omitempty"`
	BBox     *BBox   `json:"bbox,omitempty"`
	Center   *Point  `json:"center,omitempty"`
	RadiusM  float64 `json:"radius_m,omitempty"`
	Country  string  `json:"country,omitempty"`  // ISO alpha-2; default: country of the geocoded location
	Language string  `json:"language,omitempty"` // default fr

	Sources    []string `json:"sources,omitempty"`      // default: every enabled source
	GridCellKm float64  `json:"grid_cell_km,omitempty"` // 0 = auto
	MaxResults int      `json:"max_results,omitempty"`  // 0 = unlimited (bounded by server cap)
	Enrich     bool     `json:"enrich,omitempty"`       // crawl websites for emails/socials/SIRET
	// IncludeUnrelated keeps the unrelated nearby places that Google Maps
	// appends to results (filtered out by default).
	IncludeUnrelated bool   `json:"include_unrelated,omitempty"`
	WebhookURL       string `json:"webhook_url,omitempty"` // POSTed when the job ends
	Priority         int    `json:"priority,omitempty"`    // higher runs first
}

// Normalize applies defaults in place.
func (s *JobSpec) Normalize() {
	s.Query = strings.TrimSpace(s.Query)
	s.Sector = strings.ToLower(strings.TrimSpace(s.Sector))
	s.Location = strings.TrimSpace(s.Location)
	s.Country = strings.ToUpper(strings.TrimSpace(s.Country))
	// An empty country is resolved by geocoding the location (worldwide).
	s.Language = strings.ToLower(strings.TrimSpace(s.Language))
	if s.Language == "" {
		s.Language = "fr"
	}
	for i := range s.Sources {
		s.Sources[i] = strings.ToLower(strings.TrimSpace(s.Sources[i]))
	}
}

// Validate checks the spec after Normalize.
func (s *JobSpec) Validate(maxResultsCap int) error {
	var errs []error
	if s.Query == "" && s.Sector == "" {
		errs = append(errs, errors.New("query or sector is required"))
	}
	if len(s.Query) > 200 {
		errs = append(errs, errors.New("query is too long (max 200)"))
	}
	hasArea := 0
	if s.Location != "" {
		hasArea++
	}
	if s.BBox != nil {
		hasArea++
		if !s.BBox.Valid() {
			errs = append(errs, errors.New("bbox is invalid"))
		}
	}
	if s.Center != nil {
		hasArea++
		if s.RadiusM <= 0 || s.RadiusM > 100_000 {
			errs = append(errs, errors.New("radius_m must be in (0, 100000] with center"))
		}
	}
	if hasArea == 0 {
		errs = append(errs, errors.New("one of location, bbox or center+radius_m is required"))
	}
	if hasArea > 1 {
		errs = append(errs, errors.New("location, bbox and center are mutually exclusive"))
	}
	if s.Country != "" && len(s.Country) != 2 {
		errs = append(errs, errors.New("country must be an ISO 3166-1 alpha-2 code"))
	}
	if s.GridCellKm < 0 || s.GridCellKm > 50 {
		errs = append(errs, errors.New("grid_cell_km must be in [0, 50]"))
	}
	if s.MaxResults < 0 || (maxResultsCap > 0 && s.MaxResults > maxResultsCap) {
		errs = append(errs, fmt.Errorf("max_results must be in [0, %d]", maxResultsCap))
	}
	if s.WebhookURL != "" && !strings.HasPrefix(s.WebhookURL, "https://") && !strings.HasPrefix(s.WebhookURL, "http://") {
		errs = append(errs, errors.New("webhook_url must be an http(s) URL"))
	}
	return errors.Join(errs...)
}

// Counters are the job throughput metrics. They are kept separate on purpose:
// raw volume says nothing about what lands usefully in the CRM.
type Counters struct {
	TasksTotal  int64 `json:"tasks_total"`
	TasksDone   int64 `json:"tasks_done"`
	TasksFailed int64 `json:"tasks_failed"`
	RawFound    int64 `json:"raw_found"`  // every record returned by sources, duplicates included
	Unique      int64 `json:"unique"`     // distinct businesses attached to the job
	Complete    int64 `json:"complete"`   // unique records passing IsComplete
	CRMUseful   int64 `json:"crm_useful"` // unique records passing IsCRMUseful
	Enriched    int64 `json:"enriched"`   // websites crawled successfully
}

// Job is a submitted extraction request and its live state.
type Job struct {
	ID         string    `json:"id"`
	Spec       JobSpec   `json:"spec"`
	Status     JobStatus `json:"status"`
	Counters   Counters  `json:"counters"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	APIKeyID   string    `json:"-"`
}

type TaskKind string

const (
	TaskPlan    TaskKind = "plan"    // geocode + split area into search tasks
	TaskSearch  TaskKind = "search"  // query one source on one cell / page
	TaskEnrich  TaskKind = "enrich"  // crawl one website
	TaskDetails TaskKind = "details" // fetch the full place profile
)

type TaskStatus string

const (
	TaskPending TaskStatus = "pending"
	TaskRunning TaskStatus = "running"
	TaskDone    TaskStatus = "done"
	TaskFailed  TaskStatus = "failed"
)

// Task is the unit of work leased by workers.
type Task struct {
	ID        int64       `json:"id"`
	JobID     string      `json:"job_id"`
	Kind      TaskKind    `json:"kind"`
	Source    string      `json:"source,omitempty"`
	Payload   TaskPayload `json:"payload"`
	Priority  int         `json:"priority"`
	Attempts  int         `json:"attempts"`
	Status    TaskStatus  `json:"status"`
	RunAfter  time.Time   `json:"run_after"`
	LastError string      `json:"last_error,omitempty"`
}

// TaskPayload is a tagged union serialised as JSON in the queue.
type TaskPayload struct {
	// search
	Query      string `json:"q,omitempty"`
	Sector     string `json:"sector,omitempty"`
	Cell       *BBox  `json:"cell,omitempty"`
	PageToken  string `json:"page_token,omitempty"`
	Page       int    `json:"page,omitempty"`
	Language   string `json:"lang,omitempty"`
	Country    string `json:"country,omitempty"`
	Location   string `json:"location,omitempty"`
	Enrich     bool   `json:"enrich,omitempty"`
	MaxResults int    `json:"max,omitempty"`
	All        bool   `json:"all,omitempty"`
	// enrich
	BusinessID int64  `json:"business_id,omitempty"`
	Website    string `json:"website,omitempty"`
	// details
	DataID string `json:"data_id,omitempty"`
	Name   string `json:"name,omitempty"`
}
