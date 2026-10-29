package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cleads/business-extractor/internal/backend"
	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/source"
	"github.com/cleads/business-extractor/internal/store"
	"github.com/cleads/business-extractor/internal/taxonomy"
)

type empty struct{}

type jobRef struct {
	JobID string `json:"job_id" jsonschema:"job id returned by create_extraction_job or list_jobs (job_...)"`
}

func (s *srv) addTools() {
	s.catalogTools()
	s.jobTools()
	s.resultTools()
	s.diagnosticTools()
}

// ---------------------------------------------------------------- catalog

type sourcesOut struct {
	Sources []source.Info `json:"sources"`
}

type sectorBrief struct {
	Key        string   `json:"key"`
	Label      string   `json:"label"`
	GoogleType string   `json:"google_type,omitempty"`
	NAF        []string `json:"naf,omitempty"`
	Synonyms   []string `json:"synonyms,omitempty"`
}

type sectorsOut struct {
	Sectors []sectorBrief `json:"sectors"`
}

type statsOut struct {
	Stats store.Stats `json:"stats"`
}

func (s *srv) catalogTools() {
	addTool(s, &mcp.Tool{
		Name: "list_sources",
		Description: "List the data sources enabled on this server and their operating profile (official API or scraping, " +
			"grid cell size, request rate per worker, concurrency). google_maps_http (Google Maps scraping, no API key) is the " +
			"default source; osm is OpenStreetMap; sirene is the French business register (France only). Use the names in " +
			"create_extraction_job.sources to restrict a job.",
		Annotations: hints("List sources", true, false, true),
		InputSchema: closed(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, b backend.Backend, _ empty) (sourcesOut, string, error) {
		src, err := b.Sources(ctx)
		names := make([]string, len(src))
		for i, x := range src {
			names[i] = x.Name
		}
		return sourcesOut{Sources: src}, fmt.Sprintf("%d source(s) enabled: %s", len(src), strings.Join(names, ", ")), err
	})

	addTool(s, &mcp.Tool{
		Name: "list_sectors",
		Description: "List the sector taxonomy: stable keys (plumber, electrician, restaurant...) mapped to each source's " +
			"native classification (Google type, OSM tags, French NAF codes). Pass a key as create_extraction_job.sector for " +
			"precise multi-source matching (needed for SIRENE), or use a free-text query instead.",
		Annotations: hints("List sectors", true, false, true),
		InputSchema: closed(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, b backend.Backend, _ empty) (sectorsOut, string, error) {
		secs, err := b.Sectors(ctx)
		out := sectorsOut{Sectors: make([]sectorBrief, len(secs))}
		for i, x := range secs {
			out.Sectors[i] = sectorBrief{Key: x.Key, Label: x.Label, GoogleType: x.GoogleType, NAF: x.NAF, Synonyms: x.Synonyms}
		}
		return out, fmt.Sprintf("%d sectors", len(secs)), err
	})

	addTool(s, &mcp.Tool{
		Name:        "get_stats",
		Description: "Global statistics: number of businesses in the consolidated base (estimate), jobs by status, queue depth by source and running tasks.",
		Annotations: hints("Global statistics", true, false, true),
		InputSchema: closed(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, b backend.Backend, _ empty) (statsOut, string, error) {
		st, err := b.Stats(ctx)
		var pending int64
		for _, n := range st.TasksPending {
			pending += n
		}
		return statsOut{Stats: st}, fmt.Sprintf("~%d businesses in base; jobs %v; %d task(s) pending, %d running",
			st.Businesses, st.Jobs, pending, st.TasksRunning), err
	})
}

// ---------------------------------------------------------------- jobs

type createJobIn struct {
	Query            string        `json:"query,omitempty" jsonschema:"free-text activity in the job language, e.g. plombier, restaurant, dentist. One of query or sector is required"`
	Sector           string        `json:"sector,omitempty" jsonschema:"taxonomy key from list_sectors (e.g. plumber); maps the activity to each source's classification"`
	Location         string        `json:"location,omitempty" jsonschema:"place name geocoded worldwide, e.g. 'Lyon, France' or 'Dubai Marina'. Exactly one of location, bbox or center+radius_m"`
	BBox             *domain.BBox  `json:"bbox,omitempty" jsonschema:"bounding box in WGS84 degrees (alternative to location)"`
	Center           *domain.Point `json:"center,omitempty" jsonschema:"circle center (with radius_m, alternative to location)"`
	RadiusM          float64       `json:"radius_m,omitempty" jsonschema:"circle radius in meters, up to 100000 (with center)"`
	Country          string        `json:"country,omitempty" jsonschema:"ISO 3166-1 alpha-2 code. Optional: inferred from the location; set it when the place name is ambiguous"`
	Language         string        `json:"language,omitempty" jsonschema:"language of categories and reviews (fr, en, ar, ...). Default fr"`
	Sources          []string      `json:"sources,omitempty" jsonschema:"restrict to these sources (see list_sources). Default: every enabled source"`
	GridCellKm       float64       `json:"grid_cell_km,omitempty" jsonschema:"grid cell size in km; 0 = per-source default. Saturated cells are split automatically"`
	MaxResults       int           `json:"max_results,omitempty" jsonschema:"exact quota of unique businesses; the job stops when reached. 0 = no limit (server cap applies)"`
	Enrich           bool          `json:"enrich,omitempty" jsonschema:"crawl each website for emails, social profiles and SIRET. Slower, but the only way to get emails"`
	IncludeUnrelated bool          `json:"include_unrelated,omitempty" jsonschema:"keep the unrelated nearby places Google Maps appends to results (dropped by default)"`
	WebhookURL       string        `json:"webhook_url,omitempty" jsonschema:"public http(s) URL POSTed when the job ends (signed with HMAC)"`
	Priority         int           `json:"priority,omitempty" jsonschema:"higher runs first"`
	IdempotencyKey   string        `json:"idempotency_key,omitempty" jsonschema:"reuse to retry safely: the same key returns the same job instead of a new one. Generated when absent"`
}

type createJobOut struct {
	Job            domain.Job        `json:"job"`
	PlannedSources []string          `json:"planned_sources"`
	SkippedSources map[string]string `json:"skipped_sources,omitempty"`
	IdempotencyKey string            `json:"idempotency_key"`
	Replayed       bool              `json:"replayed"`
	Next           string            `json:"next"`
}

type jobOut struct {
	Job      domain.Job `json:"job"`
	Progress progress   `json:"progress"`
}

type progress struct {
	Finished       bool    `json:"finished"`
	TasksPercent   float64 `json:"tasks_percent"` // done+failed over total so far (the total grows while planning)
	ElapsedSeconds float64 `json:"elapsed_seconds"`
}

func progressOf(j domain.Job) progress {
	c := j.Counters
	p := progress{Finished: j.Status.Terminal()}
	if c.TasksTotal > 0 {
		p.TasksPercent = float64(int64(1000*float64(c.TasksDone+c.TasksFailed)/float64(c.TasksTotal))) / 10
	}
	end := time.Now()
	if !j.FinishedAt.IsZero() {
		end = j.FinishedAt
	}
	if !j.CreatedAt.IsZero() {
		p.ElapsedSeconds = float64(int64(end.Sub(j.CreatedAt).Seconds()*10)) / 10
	}
	return p
}

func jobLine(j domain.Job) string {
	c := j.Counters
	s := fmt.Sprintf("job %s %s: %d unique (%d complete, %d CRM-useful, %d enriched), tasks %d/%d done, %d failed",
		j.ID, j.Status, c.Unique, c.Complete, c.CRMUseful, c.Enriched, c.TasksDone, c.TasksTotal, c.TasksFailed)
	if j.Error != "" {
		s += "; error: " + j.Error
	}
	return s
}

type listJobsIn struct {
	Status string `json:"status,omitempty" jsonschema:"only jobs with this status"`
	Limit  int    `json:"limit,omitempty" jsonschema:"page size, default 20"`
	Before string `json:"before,omitempty" jsonschema:"next_before of the previous page (RFC 3339)"`
}

type jobBrief struct {
	ID        string           `json:"id"`
	Status    domain.JobStatus `json:"status"`
	Query     string           `json:"query,omitempty"`
	Sector    string           `json:"sector,omitempty"`
	Location  string           `json:"location,omitempty"`
	Counters  domain.Counters  `json:"counters"`
	Error     string           `json:"error,omitempty"`
	CreatedAt time.Time        `json:"created_at"`
}

type listJobsOut struct {
	Jobs       []jobBrief `json:"jobs"`
	NextBefore string     `json:"next_before,omitempty"`
}

type waitIn struct {
	JobID     string `json:"job_id" jsonschema:"job to wait for"`
	TimeoutS  int    `json:"timeout_s,omitempty" jsonschema:"maximum seconds to wait (default 60, capped by the server). The call returns earlier when the job ends"`
	MinUnique int    `json:"min_unique,omitempty" jsonschema:"also return as soon as the job has at least this many unique businesses (results are readable while it runs)"`
}

type waitOut struct {
	Job      domain.Job `json:"job"`
	Progress progress   `json:"progress"`
	TimedOut bool       `json:"timed_out"`
	Reason   string     `json:"reason"` // finished | min_unique | timeout
	WaitedS  float64    `json:"waited_s"`
	Next     string     `json:"next"`
}

func (s *srv) jobTools() {
	addTool(s, &mcp.Tool{
		Name: "create_extraction_job",
		Description: "Start an extraction job: find businesses of an activity (query or sector) in ONE area — location " +
			"(geocoded worldwide), bbox, or center + radius_m; never several. Without country the country is inferred from the " +
			"location (pass it when a name is ambiguous). language sets the language of categories and reviews (default fr). " +
			"enrich: true crawls websites and is the only way to get emails, but is slower. max_results is an exact quota. " +
			"Unrelated places Google appends are dropped unless include_unrelated. SIRENE only covers France and needs a sector. " +
			"The job runs asynchronously for seconds to many minutes (about 350 complete records/min by default); results are " +
			"readable while it runs. Returns the job id, planned and skipped sources, and the idempotency key (reuse it to retry " +
			"without creating a duplicate). Next: wait_for_job, then get_job_results.",
		Annotations: hints("Create extraction job", false, false, false),
		InputSchema: schemaFor[createJobIn](func(p map[string]*jsonschema.Schema) {
			bounds(p["radius_m"], 1, 100_000)
			bounds(p["grid_cell_km"], 0, 50)
			bounds(p["max_results"], 0, 1_000_000)
			p["country"].Pattern = "^[A-Za-z]{2}$"
			p["sources"].MaxItems = ptr(10)
		}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, b backend.Backend, in createJobIn) (createJobOut, string, error) {
		key := strings.TrimSpace(in.IdempotencyKey)
		if key == "" {
			key = newIdempotencyKey()
		}
		spec := domain.JobSpec{Query: in.Query, Sector: in.Sector, Location: in.Location, BBox: in.BBox, Center: in.Center,
			RadiusM: in.RadiusM, Country: in.Country, Language: in.Language, Sources: in.Sources, GridCellKm: in.GridCellKm,
			MaxResults: in.MaxResults, Enrich: in.Enrich, IncludeUnrelated: in.IncludeUnrelated, WebhookURL: in.WebhookURL,
			Priority: in.Priority}
		res, err := b.CreateJob(ctx, spec, key)
		if err != nil {
			return createJobOut{}, "", err
		}
		out := createJobOut{Job: res.Job, PlannedSources: res.PlannedSources, SkippedSources: res.SkippedSources,
			IdempotencyKey: key, Replayed: res.Replayed,
			Next: fmt.Sprintf("call wait_for_job with job_id %q, then get_job_results", res.Job.ID)}
		verb := "created"
		if res.Replayed {
			verb = "already existed (same idempotency key)"
		}
		sum := fmt.Sprintf("job %s %s, status %s, sources: %s", res.Job.ID, verb, res.Job.Status, strings.Join(res.PlannedSources, ", "))
		if len(res.SkippedSources) > 0 {
			sum += fmt.Sprintf("; skipped: %v", res.SkippedSources)
		}
		return out, sum, nil
	})

	addTool(s, &mcp.Tool{
		Name: "get_job",
		Description: "Get a job's status (queued, running, completed, failed, cancelled) and counters: raw_found (everything " +
			"sources returned), unique (after deduplication), complete (identity + address + phone + website/email), " +
			"crm_useful (operating and reachable), enriched, and task progress. Cheap: call it as often as needed.",
		Annotations: hints("Get job", true, false, true),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, b backend.Backend, in jobRef) (jobOut, string, error) {
		j, err := b.GetJob(ctx, in.JobID)
		if err != nil {
			return jobOut{}, "", err
		}
		return jobOut{Job: j, Progress: progressOf(j)}, jobLine(j), nil
	})

	addTool(s, &mcp.Tool{
		Name:        "list_jobs",
		Description: "List this API key's jobs, newest first, with status and counters. Paginate with next_before.",
		Annotations: hints("List jobs", true, false, true),
		InputSchema: schemaFor[listJobsIn](func(p map[string]*jsonschema.Schema) {
			enum(p["status"], "queued", "running", "completed", "failed", "cancelled")
			bounds(p["limit"], 1, 50)
		}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, b backend.Backend, in listJobsIn) (listJobsOut, string, error) {
		var before time.Time
		if in.Before != "" {
			t, err := time.Parse(time.RFC3339Nano, in.Before)
			if err != nil {
				return listJobsOut{}, "", inputErr("before must be an RFC 3339 time (next_before of the previous page)")
			}
			before = t
		}
		limit := in.Limit
		if limit == 0 {
			limit = 20
		}
		res, err := b.ListJobs(ctx, in.Status, min(limit, 50), before)
		if err != nil {
			return listJobsOut{}, "", err
		}
		out := listJobsOut{Jobs: make([]jobBrief, len(res.Jobs)), NextBefore: res.NextBefore}
		for i, j := range res.Jobs {
			out.Jobs[i] = jobBrief{ID: j.ID, Status: j.Status, Query: j.Spec.Query, Sector: j.Spec.Sector, Location: j.Spec.Location,
				Counters: j.Counters, Error: j.Error, CreatedAt: j.CreatedAt}
		}
		return out, fmt.Sprintf("%d job(s)", len(out.Jobs)), nil
	})

	addTool(s, &mcp.Tool{
		Name: "wait_for_job",
		Description: fmt.Sprintf("Wait until a job finishes (or has min_unique results), sending MCP progress notifications "+
			"from its counters. Waits at most timeout_s (default 60, capped at %d): if the job is still running it returns "+
			"the current state with timed_out=true, and you can call it again or read partial results with get_job_results. "+
			"Large jobs take several minutes because every place detail and website is fetched before the job completes.",
			int(s.opts.MaxWait.Seconds())),
		Annotations: hints("Wait for job", true, false, true),
		InputSchema: schemaFor[waitIn](func(p map[string]*jsonschema.Schema) {
			bounds(p["timeout_s"], 1, s.opts.MaxWait.Seconds())
			bounds(p["min_unique"], 0, 1_000_000)
		}),
	}, s.waitForJob)

	addTool(s, &mcp.Tool{
		Name:        "cancel_job",
		Description: "Cancel a queued or running job: pending tasks are dropped and no new result is added (results found so far stay readable). Irreversible. Fails with conflict if the job already ended.",
		Annotations: hints("Cancel job", false, true, true),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, b backend.Backend, in jobRef) (jobOut, string, error) {
		j, err := b.CancelJob(ctx, in.JobID)
		if err != nil {
			return jobOut{}, "", err
		}
		return jobOut{Job: j, Progress: progressOf(j)}, jobLine(j), nil
	})
}

func (s *srv) waitForJob(ctx context.Context, req *mcp.CallToolRequest, b backend.Backend, in waitIn) (waitOut, string, error) {
	timeout := time.Duration(in.TimeoutS) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	timeout = min(timeout, s.opts.MaxWait)
	start := time.Now()
	deadline := start.Add(timeout)
	tok := req.Params.GetProgressToken()
	var last domain.Counters
	lastStatus := domain.JobStatus("")
	t := time.NewTicker(s.opts.PollEvery)
	defer t.Stop()
	for {
		j, err := b.GetJob(ctx, in.JobID)
		if err != nil {
			return waitOut{}, "", err
		}
		if tok != nil && (j.Counters != last || j.Status != lastStatus) {
			c := j.Counters
			_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
				ProgressToken: tok, Progress: float64(c.TasksDone + c.TasksFailed), Total: float64(c.TasksTotal),
				Message: fmt.Sprintf("%s: %d unique, %d complete, %d CRM-useful, tasks %d/%d", j.Status, c.Unique, c.Complete,
					c.CRMUseful, c.TasksDone+c.TasksFailed, c.TasksTotal),
			})
			last, lastStatus = j.Counters, j.Status
		}
		out := waitOut{Job: j, Progress: progressOf(j), WaitedS: float64(int64(time.Since(start).Seconds()*10)) / 10}
		switch {
		case j.Status.Terminal():
			out.Reason = "finished"
			out.Next = "read results with get_job_results or export_job"
			if j.Status == domain.JobFailed || j.Counters.TasksFailed > 0 {
				out.Next += "; explain failures with get_job_errors"
			}
		case in.MinUnique > 0 && j.Counters.Unique >= int64(in.MinUnique):
			out.Reason = "min_unique"
			out.Next = "results are readable now with get_job_results; call wait_for_job again for the rest"
		case !time.Now().Before(deadline):
			out.Reason, out.TimedOut = "timeout", true
			out.Next = "still running: call wait_for_job again, or read partial results with get_job_results"
		}
		if out.Reason != "" {
			return out, out.Reason + " — " + jobLine(j), nil
		}
		select {
		case <-ctx.Done():
			return waitOut{}, "", ctx.Err()
		case <-t.C:
		}
	}
}

// ---------------------------------------------------------------- results

type resultsIn struct {
	JobID          string   `json:"job_id" jsonschema:"job to read"`
	Cursor         string   `json:"cursor,omitempty" jsonschema:"next_cursor of the previous page; omit for the first page"`
	Limit          int      `json:"limit,omitempty" jsonschema:"records per page, default 20 (pages are also cut to stay small)"`
	Fields         []string `json:"fields,omitempty" jsonschema:"fields to return (default: a compact set: id, name, category, address, city, phone, website, emails, socials, rating, completeness...). Ask for details, hours, lat, lng, siret... explicitly"`
	IncludeReviews bool     `json:"include_reviews,omitempty" jsonschema:"include up to 3 short reviews per record (personal data of third parties: only when needed)"`
	Filters
}

type pageOut struct {
	Items      []map[string]any `json:"items"`
	Returned   int              `json:"returned"`
	NextCursor string           `json:"next_cursor"`
	HasMore    bool             `json:"has_more"`
	Truncated  bool             `json:"truncated"` // page cut to fit the size budget
	JobStatus  domain.JobStatus `json:"job_status,omitempty"`
	Counters   *domain.Counters `json:"counters,omitempty"`
}

type exportIn struct {
	JobID       string `json:"job_id" jsonschema:"job to export"`
	Format      string `json:"format,omitempty" jsonschema:"csv (default), csv-excel (semicolons + BOM for French Excel) or jsonl (full records)"`
	PreviewRows int    `json:"preview_rows,omitempty" jsonschema:"rows shown in the preview, default 5"`
	Filters
}

// content adds a resource link next to the text blocks.
func (o exportOut) content() []mcp.Content {
	return []mcp.Content{&mcp.ResourceLink{URI: o.ResourceURI, Name: "export." + extOf(o.Format),
		Title: "Job export (" + o.Format + ")", MIMEType: mimeOf(o.Format),
		Description: "First chunk; follow _meta.next_uri of each chunk until done"}}
}

type exportOut struct {
	ResourceURI string           `json:"resource_uri"`
	DownloadURL string           `json:"download_url"`
	Auth        string           `json:"auth"`
	Format      string           `json:"format"`
	Preview     string           `json:"preview"`
	PreviewRows int              `json:"preview_rows"`
	JobStatus   domain.JobStatus `json:"job_status"`
	Unique      int64            `json:"unique"`
	Note        string           `json:"note"`
}

type searchIn struct {
	Cursor string   `json:"cursor,omitempty" jsonschema:"next_cursor of the previous page"`
	Limit  int      `json:"limit,omitempty" jsonschema:"records per page, default 20"`
	Fields []string `json:"fields,omitempty" jsonschema:"fields to return (default: compact set)"`
	Filters
}

type businessIn struct {
	BusinessID     int64 `json:"business_id" jsonschema:"business id (the id field of a result)"`
	IncludeDetails bool  `json:"include_details,omitempty" jsonschema:"include the full place profile: description, owner, attributes, links, hours (photos capped)"`
	IncludeReviews bool  `json:"include_reviews,omitempty" jsonschema:"include up to 3 short reviews (personal data of third parties: only when needed)"`
}

type businessOut struct {
	Business  map[string]any `json:"business"`
	Truncated []string       `json:"truncated,omitempty"` // parts removed to fit the size budget
}

func size(v any) int {
	b, _ := json.Marshal(v)
	return len(b)
}

func resultsSchema[T any](f func(p map[string]*jsonschema.Schema)) *jsonschema.Schema {
	return schemaFor[T](func(p map[string]*jsonschema.Schema) {
		bounds(p["limit"], 1, 100)
		bounds(p["min_completeness"], 0, 100)
		if fs := p["fields"]; fs != nil && fs.Items != nil {
			fs.MaxItems = ptr(40)
			for _, n := range businessFields() {
				fs.Items.Enum = append(fs.Items.Enum, n)
			}
		}
		if f != nil {
			f(p)
		}
	})
}

func parseCursor(c string) (int64, error) {
	if c == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(c, 10, 64)
	if err != nil || n < 0 {
		return 0, inputErr("cursor must be the next_cursor value of a previous page")
	}
	return n, nil
}

func (s *srv) resultTools() {
	addTool(s, &mcp.Tool{
		Name: "get_job_results",
		Description: "Read a job's businesses page by page (stable cursor: works while the job runs and only returns new " +
			"records). Compact by default (no reviews, photos or raw ids) to save context; choose fields to project, filters to " +
			"narrow (useful, has_phone, has_email, min_completeness, city, postal_code, category, q). Pages are capped at 100 " +
			"records and about 24 KB (truncated=true when cut). For a full list, use export_job instead of paging everything.",
		Annotations: hints("Get job results", true, false, true),
		InputSchema: resultsSchema[resultsIn](nil),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, b backend.Backend, in resultsIn) (pageOut, string, error) {
		cursor, err := parseCursor(in.Cursor)
		if err != nil {
			return pageOut{}, "", err
		}
		proj, err := newProjector(in.Fields, in.IncludeReviews)
		if err != nil {
			return pageOut{}, "", inputErr("%s", err.Error())
		}
		limit := in.Limit
		if limit == 0 {
			limit = 20
		}
		page, err := b.JobBusinesses(ctx, in.JobID, cursor, min(limit, 100), in.Filters.store())
		if err != nil {
			return pageOut{}, "", err
		}
		out := pageOut{Items: make([]map[string]any, len(page.Items)), JobStatus: page.JobStatus, Counters: &page.Counters,
			NextCursor: page.NextCursor, HasMore: page.HasMore}
		for i, r := range page.Items {
			out.Items[i] = proj.project(r.Business, r.Seq)
		}
		if n := fit(out.Items, maxStructuredBytes); n < len(out.Items) {
			out.Items, out.Truncated, out.HasMore = out.Items[:n], true, true
			out.NextCursor = strconv.FormatInt(page.Items[n-1].Seq, 10)
		}
		out.Returned = len(out.Items)
		sum := fmt.Sprintf("%d record(s) from job %s (%s, %d unique so far)", out.Returned, in.JobID, page.JobStatus, page.Counters.Unique)
		if out.HasMore {
			sum += "; more with cursor " + out.NextCursor
		}
		return out, sum, nil
	})

	addTool(s, &mcp.Tool{
		Name: "export_job",
		Description: "Export a job's businesses as CSV, Excel-friendly CSV or JSONL without loading them into the conversation: " +
			"returns a resource link (readable in chunks through resources/read, cursor in the URI) and an authenticated " +
			"download URL (send the same API key as Authorization: Bearer), plus a short preview. Works while the job runs " +
			"(the file then holds the results so far). Same filters as get_job_results.",
		Annotations: hints("Export job", true, false, true),
		InputSchema: schemaFor[exportIn](func(p map[string]*jsonschema.Schema) {
			enum(p["format"], "csv", "csv-excel", "jsonl")
			bounds(p["preview_rows"], 0, 20)
			bounds(p["min_completeness"], 0, 100)
		}),
	}, func(ctx context.Context, req *mcp.CallToolRequest, b backend.Backend, in exportIn) (exportOut, string, error) {
		format := in.Format
		if format == "" {
			format = "csv"
		}
		rows := in.PreviewRows
		if rows == 0 {
			rows = 5
		}
		j, err := b.GetJob(ctx, in.JobID)
		if err != nil {
			return exportOut{}, "", err
		}
		out := exportOut{Format: format, JobStatus: j.Status, Unique: j.Counters.Unique,
			ResourceURI: exportURI(j.ID, format, 0, in.Filters), DownloadURL: s.downloadURL(j.ID, format, in.Filters),
			Auth: "send your API key: Authorization: Bearer <key> (or X-API-Key)",
			Note: "read the full export with resources/read on resource_uri, following _meta.next_uri until done; or download it"}
		if rows > 0 {
			ch, err := b.ExportChunk(ctx, j.ID, format, in.Filters.store(), 0, rows, 8<<10)
			if err != nil {
				return exportOut{}, "", err
			}
			out.Preview, out.PreviewRows = ch.Text, ch.Rows
		}
		return out, fmt.Sprintf("export of job %s (%s, %d unique, job %s): %s", j.ID, format, j.Counters.Unique, j.Status, out.ResourceURI), nil
	})

	addTool(s, &mcp.Tool{
		Name: "search_prospect_base",
		Description: "Search the consolidated prospect base: every business found by every job, deduplicated across " +
			"sources. Same filters and compact projection as get_job_results; cursor-paginated by id. Use it to reuse earlier " +
			"extractions before launching a new job.",
		Annotations: hints("Search prospect base", true, false, true),
		InputSchema: resultsSchema[searchIn](nil),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, b backend.Backend, in searchIn) (pageOut, string, error) {
		cursor, err := parseCursor(in.Cursor)
		if err != nil {
			return pageOut{}, "", err
		}
		proj, err := newProjector(in.Fields, false)
		if err != nil {
			return pageOut{}, "", inputErr("%s", err.Error())
		}
		limit := in.Limit
		if limit == 0 {
			limit = 20
		}
		page, err := b.SearchBusinesses(ctx, cursor, min(limit, 100), in.Filters.store())
		if err != nil {
			return pageOut{}, "", err
		}
		out := pageOut{Items: make([]map[string]any, len(page.Items)), NextCursor: page.NextCursor, HasMore: page.HasMore}
		for i, x := range page.Items {
			out.Items[i] = proj.project(x, 0)
		}
		if n := fit(out.Items, maxStructuredBytes); n < len(out.Items) {
			out.Items, out.Truncated, out.HasMore = out.Items[:n], true, true
			out.NextCursor = strconv.FormatInt(page.Items[n-1].ID, 10)
		}
		out.Returned = len(out.Items)
		sum := fmt.Sprintf("%d business(es) from the prospect base", out.Returned)
		if out.HasMore {
			sum += "; more with cursor " + out.NextCursor
		}
		return out, sum, nil
	})

	addTool(s, &mcp.Tool{
		Name: "get_business",
		Description: "Get one business of the consolidated base by id. Compact by default; include_details adds the full " +
			"place profile (description, owner, attributes, links, opening hours), include_reviews up to 3 short reviews.",
		Annotations: hints("Get business", true, false, true),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, b backend.Backend, in businessIn) (businessOut, string, error) {
		x, err := b.GetBusiness(ctx, in.BusinessID)
		if err != nil {
			return businessOut{}, "", err
		}
		fields := compactFields
		if in.IncludeDetails {
			fields = append(append([]string(nil), compactFields...), "categories", "street", "region", "lat", "lng", "domain",
				"hours", "price_level", "siren", "siret", "naf", "sources", "source_url", "details", "enriched_at", "first_seen_at", "last_seen_at")
		}
		proj, _ := newProjector(fields, in.IncludeReviews)
		m := proj.project(x, 0)
		out := businessOut{Business: m}
		// A rich profile can exceed the budget: drop the bulkiest parts first.
		for _, k := range []string{"attributes", "images", "reviews"} {
			if size(out) <= maxStructuredBytes {
				break
			}
			if d, ok := m["details"].(map[string]any); ok {
				delete(d, k)
				out.Truncated = append(out.Truncated, "details."+k)
			}
		}
		if size(out) > maxStructuredBytes {
			delete(m, "details")
			out.Truncated = append(out.Truncated, "details")
		}
		return out, fmt.Sprintf("business %d: %s, %s", x.ID, x.Name, x.City), nil
	})
}

// exportURI is the resource URI of an export chunk.
func exportURI(jobID, format string, cursor int64, f Filters) string {
	u := "cleads://jobs/" + url.PathEscape(jobID) + "/export/" + format + "?cursor=" + strconv.FormatInt(cursor, 10)
	if q := f.query().Encode(); q != "" {
		u += "&filter=" + url.QueryEscape(q)
	}
	return u
}

func (s *srv) downloadURL(jobID, format string, f Filters) string {
	q := f.query()
	q.Set("format", format)
	return s.opts.PublicURL + "/v1/jobs/" + url.PathEscape(jobID) + "/export?" + q.Encode()
}

// ---------------------------------------------------------------- diagnostics

type logsIn struct {
	Level  string `json:"level,omitempty" jsonschema:"minimum level, default warn"`
	JobID  string `json:"job_id,omitempty" jsonschema:"only this job's events"`
	Source string `json:"source,omitempty" jsonschema:"queue name (google_maps_http.details) or prefix ending with * (google_maps_http*)"`
	Since  string `json:"since,omitempty" jsonschema:"RFC 3339 time or duration back from now (15m, 2h); default 1h"`
	Limit  int    `json:"limit,omitempty" jsonschema:"events to return, default 50"`
	Local  bool   `json:"local,omitempty" jsonschema:"read the recent in-memory logs (INFO and above) of the replica serving this call instead of the shared events table"`
}

type healthOut struct {
	Health backend.HealthSummary `json:"health"`
}

func (s *srv) diagnosticTools() {
	addTool(s, &mcp.Tool{
		Name: "get_job_errors",
		Description: "Explain why a job (or some of its tasks) failed: failed tasks, tasks being retried and the job's " +
			"warnings/errors from every worker, grouped by cause, e.g. \"12 × http 500 on google_maps_http.details\". " +
			"Start any diagnosis here; then get_system_health to see whether a provider is blocking us.",
		Annotations: hints("Get job errors", true, false, true),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, b backend.Backend, in jobRef) (backend.JobErrors, string, error) {
		je, err := b.JobErrors(ctx, in.JobID)
		if err != nil {
			return je, "", err
		}
		if n := fit(je.Groups, maxStructuredBytes-2048); n < len(je.Groups) {
			je.Groups = je.Groups[:n]
		}
		return je, fmt.Sprintf("job %s (%s), %d failed task(s): %s", je.JobID, je.Status, je.TasksFailed, je.Summary), nil
	})

	addTool(s, &mcp.Tool{
		Name: "get_logs",
		Description: "Read warnings and errors logged by every API and worker process (shared events table, kept 7 days by " +
			"default), newest first, filtered by level, job, source and time; secrets are masked. Requires the admin API key " +
			"(logs mix every client): other keys get a forbidden error — use get_job_errors for your own jobs.",
		Annotations: hints("Get logs", true, false, true),
		InputSchema: schemaFor[logsIn](func(p map[string]*jsonschema.Schema) {
			enum(p["level"], "debug", "info", "warn", "error")
			bounds(p["limit"], 1, 200)
		}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, b backend.Backend, in logsIn) (backend.LogList, string, error) {
		q := backend.LogQuery{Level: in.Level, JobID: in.JobID, Source: in.Source, Limit: in.Limit, Local: in.Local}
		if q.Limit == 0 {
			q.Limit = 50
		}
		since := in.Since
		if since == "" {
			since = "1h"
		}
		if d, err := time.ParseDuration(since); err == nil && d > 0 {
			q.Since = time.Now().Add(-d)
		} else if t, err := time.Parse(time.RFC3339Nano, since); err == nil {
			q.Since = t
		} else {
			return backend.LogList{}, "", inputErr("since must be a duration (15m) or an RFC 3339 time")
		}
		res, err := b.Logs(ctx, q)
		if err != nil {
			return res, "", err
		}
		for i := range res.Events {
			for k, v := range res.Events[i].Attrs {
				if str, ok := v.(string); ok {
					res.Events[i].Attrs[k] = clip(str, 500)
				}
			}
		}
		total := len(res.Events)
		if n := fit(res.Events, maxStructuredBytes); n < total {
			res.Events = res.Events[:n]
		}
		sum := fmt.Sprintf("%d event(s) from %s", len(res.Events), res.Scope)
		if len(res.Events) < total {
			sum += fmt.Sprintf(" (%d more cut to fit; narrow the filters)", total-len(res.Events))
		}
		return res, sum, nil
	})

	addTool(s, &mcp.Tool{
		Name: "get_system_health",
		Description: "Health summary of the whole deployment: database, live workers, queue depth, and per source the " +
			"circuit-breaker state, adaptive vs. configured request rate, outcomes of the last 5 minutes (ok, errors, " +
			"rate-limited, blocked) and a verdict (ok, idle, throttled, blocked, failing, no_worker). Directly answers " +
			"\"is Google blocking us?\" (source google_maps_http). The admin key also gets the latest errors.",
		Annotations: hints("System health", true, false, true),
		InputSchema: closed(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, b backend.Backend, _ empty) (healthOut, string, error) {
		h, err := b.Health(ctx)
		if err != nil {
			return healthOut{}, "", err
		}
		for i := range h.RecentErrors {
			h.RecentErrors[i].Attrs = nil // the msg, source and job are enough here; get_logs has the rest
		}
		return healthOut{Health: h}, h.Status + ": " + strings.ReplaceAll(h.Summary, "\n", "; "), nil
	})
}

func ptr[T any](v T) *T { return &v }

// sectorKeys is used by prompts to suggest valid sector keys.
func sectorKeys() []string {
	all := taxonomy.All()
	out := make([]string, len(all))
	for i, s := range all {
		out[i] = s.Key
	}
	return out
}
