package mcpserver_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cleads/business-extractor/internal/api"
	"github.com/cleads/business-extractor/internal/backend"
	"github.com/cleads/business-extractor/internal/config"
	"github.com/cleads/business-extractor/internal/engine"
	"github.com/cleads/business-extractor/internal/geocode"
	"github.com/cleads/business-extractor/internal/mcpserver"
	"github.com/cleads/business-extractor/internal/metrics"
	"github.com/cleads/business-extractor/internal/obs"
	"github.com/cleads/business-extractor/internal/source"
	"github.com/cleads/business-extractor/internal/source/mock"
	"github.com/cleads/business-extractor/internal/store/memstore"
)

type env struct {
	api      *api.Server
	progress chan *mcp.ProgressNotificationParams
}

// startAPI runs an engine (mock source, memstore, teed logs) and returns the
// API server whose in-process backends the MCP tools use.
func startAPI(t *testing.T) *api.Server {
	t.Helper()
	cfg := &config.Config{MaxResultsCap: 10_000, RatePerKey: 1000, MaxBodyBytes: 1 << 20}
	log, tee := obs.NewLogger(io.Discard, "json", slog.LevelInfo, obs.TeeOptions{Worker: "test", RingSize: 100, FlushEvery: 10 * time.Millisecond})
	st := memstore.New()
	m := metrics.New()
	eng := engine.New(engine.Config{PollMin: 5 * time.Millisecond, PollMax: 20 * time.Millisecond, FinalizeEvery: 20 * time.Millisecond,
		WorkerID: "w1", StatusEvery: 20 * time.Millisecond},
		st, []source.Source{mock.New(mock.Config{CellKm: 1, Latency: 5 * time.Millisecond})},
		geocode.Static{"lyon": {MinLat: 45.74, MinLng: 4.80, MaxLat: 45.76, MaxLng: 4.83}}, nil, m, log)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = eng.Run(ctx) }()
	go func() { defer wg.Done(); tee.Run(ctx, st) }()
	t.Cleanup(func() { cancel(); wg.Wait() })
	srv := api.New(cfg, st, eng, m, log, "test")
	srv.SetLogRing(tee.Ring())
	return srv
}

// connect opens an in-memory MCP session acting as API key key.
func connect(t *testing.T, srv *api.Server, key string) (*mcp.ClientSession, chan *mcp.ProgressNotificationParams) {
	t.Helper()
	server := mcpserver.New(mcpserver.Options{
		Backend:   func(*mcp.RequestExtra) (backend.Backend, error) { return srv.Backend(key), nil },
		Version:   "test",
		PublicURL: "https://api.example",
		MaxWait:   20 * time.Second,
		PollEvery: 20 * time.Millisecond,
	})
	progress := make(chan *mcp.ProgressNotificationParams, 1000)
	client := mcp.NewClient(&mcp.Implementation{Name: "test"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, r *mcp.ProgressNotificationClientRequest) {
			select {
			case progress <- r.Params:
			default:
			}
		},
	})
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close(); _ = ss.Wait() })
	return cs, progress
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if out != nil && !res.IsError {
		b, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
	}
	return res
}

func text(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

func structuredSize(res *mcp.CallToolResult) int {
	b, _ := json.Marshal(res.StructuredContent)
	return len(b)
}

func TestToolCatalog(t *testing.T) {
	cs, _ := connect(t, startAPI(t), "")
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"list_sources", "list_sectors", "create_extraction_job", "get_job", "list_jobs", "wait_for_job",
		"get_job_results", "export_job", "search_prospect_base", "get_business", "cancel_job", "get_job_errors",
		"get_logs", "get_system_health", "get_stats"}
	got := map[string]*mcp.Tool{}
	for _, tl := range tools.Tools {
		got[tl.Name] = tl
	}
	for _, n := range want {
		tl, ok := got[n]
		if !ok {
			t.Fatalf("missing tool %s", n)
		}
		if tl.Annotations == nil || tl.Annotations.DestructiveHint == nil || tl.Annotations.OpenWorldHint == nil || len(tl.Description) < 60 {
			t.Errorf("%s: annotations or description missing", n)
		}
		if tl.OutputSchema == nil {
			t.Errorf("%s: no output schema", n)
		}
		in, _ := json.Marshal(tl.InputSchema)
		if strings.Contains(string(in), `"null"`) {
			t.Errorf("%s: input schema has a nullable type (not portable): %s", n, in)
		}
		if !strings.Contains(string(in), `"additionalProperties":{"not":{}}`) && !strings.Contains(string(in), `"additionalProperties":false`) {
			t.Errorf("%s: input schema is not strict: %s", n, in)
		}
	}
	if !*got["cancel_job"].Annotations.DestructiveHint || got["cancel_job"].Annotations.ReadOnlyHint {
		t.Error("cancel_job must be destructive")
	}
	if !got["get_job"].Annotations.ReadOnlyHint || got["create_extraction_job"].Annotations.ReadOnlyHint {
		t.Error("read-only hints")
	}
	desc := got["create_extraction_job"].Description
	for _, rule := range []string{"ONE area", "country", "language", "enrich", "exact quota", "include_unrelated", "SIRENE only covers France", "readable while it runs"} {
		if !strings.Contains(desc, rule) {
			t.Errorf("create_extraction_job description lacks %q", rule)
		}
	}
	// Strict input: unknown properties are rejected.
	res := call(t, cs, "get_job", map[string]any{"job_id": "x", "bogus": 1}, nil)
	if !res.IsError {
		t.Error("unknown property accepted")
	}
}

func TestAgentFlow(t *testing.T) {
	cs, progress := connect(t, startAPI(t), "crm")
	ctx := context.Background()

	var created struct {
		Job            struct{ ID string } `json:"job"`
		PlannedSources []string            `json:"planned_sources"`
		IdempotencyKey string              `json:"idempotency_key"`
		Replayed       bool                `json:"replayed"`
	}
	res := call(t, cs, "create_extraction_job", map[string]any{"query": "plombier", "location": "Lyon"}, &created)
	if res.IsError || created.Job.ID == "" || created.IdempotencyKey == "" || !slices.Equal(created.PlannedSources, []string{"mock"}) {
		t.Fatalf("create: %s", text(res))
	}
	id := created.Job.ID
	// Replaying the returned key returns the same job.
	var again struct {
		Job      struct{ ID string } `json:"job"`
		Replayed bool                `json:"replayed"`
	}
	call(t, cs, "create_extraction_job", map[string]any{"query": "plombier", "location": "Lyon", "idempotency_key": created.IdempotencyKey}, &again)
	if again.Job.ID != id || !again.Replayed {
		t.Fatalf("idempotent replay: %+v", again)
	}
	// Business rule enforced: one area shape only.
	res = call(t, cs, "create_extraction_job", map[string]any{"query": "x", "location": "Lyon", "center": map[string]any{"lat": 1, "lng": 1}, "radius_m": 500}, nil)
	if !res.IsError || !strings.Contains(text(res), "mutually exclusive") {
		t.Fatalf("two areas accepted: %s", text(res))
	}

	// wait with progress notifications
	params := &mcp.CallToolParams{Name: "wait_for_job", Arguments: map[string]any{"job_id": id, "timeout_s": 20}}
	params.SetProgressToken("tok-1")
	wres, err := cs.CallTool(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	var waited struct {
		Job struct {
			Status   string `json:"status"`
			Counters struct {
				Unique int `json:"unique"`
			} `json:"counters"`
		} `json:"job"`
		Reason   string `json:"reason"`
		TimedOut bool   `json:"timed_out"`
	}
	b, _ := json.Marshal(wres.StructuredContent)
	_ = json.Unmarshal(b, &waited)
	if waited.Reason != "finished" || waited.Job.Status != "completed" || waited.Job.Counters.Unique == 0 {
		t.Fatalf("wait: %s", text(wres))
	}
	n := len(progress)
	if n == 0 {
		t.Fatal("no progress notification")
	}
	var last *mcp.ProgressNotificationParams
	for range n {
		p := <-progress
		if p.ProgressToken != "tok-1" || (last != nil && p.Progress < last.Progress) || p.Message == "" {
			t.Fatalf("bad progress %+v after %+v", p, last)
		}
		last = p
	}
	if last.Total == 0 || last.Progress != last.Total {
		t.Fatalf("final progress %v/%v", last.Progress, last.Total)
	}

	// results: compact by default, projection on demand, size bound
	var page struct {
		Items      []map[string]any `json:"items"`
		NextCursor string           `json:"next_cursor"`
		HasMore    bool             `json:"has_more"`
		Truncated  bool             `json:"truncated"`
	}
	res = call(t, cs, "get_job_results", map[string]any{"job_id": id, "limit": 5}, &page)
	if len(page.Items) != 5 || !page.HasMore {
		t.Fatalf("page: %s", text(res))
	}
	for _, k := range []string{"dedup_key", "details", "lat", "sources", "place_id"} {
		if _, ok := page.Items[0][k]; ok {
			t.Fatalf("compact mode leaks %q: %v", k, page.Items[0])
		}
	}
	if page.Items[0]["name"] == nil || page.Items[0]["seq"] == nil {
		t.Fatalf("compact mode misses basics: %v", page.Items[0])
	}
	var proj struct {
		Items []map[string]any `json:"items"`
	}
	call(t, cs, "get_job_results", map[string]any{"job_id": id, "limit": 3, "fields": []string{"name", "phone"}, "cursor": page.NextCursor, "has_phone": true}, &proj)
	if len(proj.Items) != 3 {
		t.Fatalf("projection page: %v", proj)
	}
	for _, it := range proj.Items {
		if len(it) > 2 || it["phone"] == "" {
			t.Fatalf("projection/filter: %v", it)
		}
	}
	res = call(t, cs, "get_job_results", map[string]any{"job_id": id, "fields": []string{"nope"}}, nil)
	if !res.IsError {
		t.Fatal("unknown field accepted")
	}
	all := []string{"id", "seq", "name", "category", "categories", "address", "street", "postal_code", "city", "region", "country",
		"lat", "lng", "phone", "website", "domain", "emails", "socials", "rating", "reviews_count", "hours", "status", "sources",
		"place_id", "cid", "data_id", "completeness", "first_seen_at", "last_seen_at", "dedup_key"}
	page.Items = nil
	res = call(t, cs, "get_job_results", map[string]any{"job_id": id, "limit": 100, "fields": all}, &page)
	if sz := structuredSize(res); sz > 26<<10 {
		t.Fatalf("structured result is %d bytes", sz)
	}
	if !page.Truncated || len(page.Items) == 100 {
		t.Fatalf("expected a truncated page, got %d items", len(page.Items))
	}

	// export: resource link + chunked reads add up to the whole job
	var exp struct {
		ResourceURI string `json:"resource_uri"`
		DownloadURL string `json:"download_url"`
		Preview     string `json:"preview"`
		PreviewRows int    `json:"preview_rows"`
		Unique      int    `json:"unique"`
	}
	res = call(t, cs, "export_job", map[string]any{"job_id": id, "format": "csv"}, &exp)
	if exp.PreviewRows != 5 || !strings.HasPrefix(exp.Preview, "id,name,") || !strings.HasPrefix(exp.DownloadURL, "https://api.example/v1/jobs/"+id+"/export?") {
		t.Fatalf("export: %s", text(res))
	}
	var link *mcp.ResourceLink
	for _, c := range res.Content {
		if l, ok := c.(*mcp.ResourceLink); ok {
			link = l
		}
	}
	if link == nil || link.URI != exp.ResourceURI {
		t.Fatal("export_job returned no resource link")
	}
	rows, headers, uri := 0, 0, exp.ResourceURI
	for i := 0; uri != "" && i < 100; i++ {
		rr, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
		if err != nil {
			t.Fatalf("read %s: %v", uri, err)
		}
		c := rr.Contents[0]
		if len(c.Text) > 49<<10 {
			t.Fatalf("chunk of %d bytes", len(c.Text))
		}
		for _, line := range strings.Split(strings.TrimRight(c.Text, "\n"), "\n") {
			if strings.HasPrefix(line, "id,name,") {
				headers++
			} else if line != "" {
				rows++
			}
		}
		uri, _ = c.Meta["next_uri"].(string)
	}
	if headers != 1 || rows != exp.Unique {
		t.Fatalf("export chunks: %d headers, %d rows, want %d", headers, rows, exp.Unique)
	}

	// prospect base and one business
	var base struct {
		Items []map[string]any `json:"items"`
	}
	call(t, cs, "search_prospect_base", map[string]any{"limit": 2, "has_phone": true}, &base)
	if len(base.Items) != 2 {
		t.Fatalf("base: %v", base)
	}
	var biz struct {
		Business map[string]any `json:"business"`
	}
	res = call(t, cs, "get_business", map[string]any{"business_id": base.Items[0]["id"], "include_details": true}, &biz)
	if biz.Business["name"] == nil || biz.Business["sources"] == nil {
		t.Fatalf("business: %s", text(res))
	}

	// diagnostics and catalog
	for _, tool := range []string{"get_system_health", "get_stats", "list_sources", "list_sectors", "list_jobs"} {
		if res := call(t, cs, tool, map[string]any{}, nil); res.IsError || structuredSize(res) > 26<<10 {
			t.Fatalf("%s: %s", tool, text(res))
		}
	}
	var je backend.JobErrors
	call(t, cs, "get_job_errors", map[string]any{"job_id": id}, &je)
	if je.JobID != id || je.Summary == "" {
		t.Fatalf("job errors: %+v", je)
	}
	res = call(t, cs, "cancel_job", map[string]any{"job_id": id}, nil)
	if !res.IsError || !strings.Contains(text(res), "conflict") {
		t.Fatalf("cancel finished job: %s", text(res))
	}
}

func TestIsolationAndAdmin(t *testing.T) {
	srv := startAPI(t)
	crm, _ := connect(t, srv, "crm")
	other, _ := connect(t, srv, "other")
	admin, _ := connect(t, srv, "admin")

	var created struct {
		Job struct{ ID string } `json:"job"`
	}
	call(t, crm, "create_extraction_job", map[string]any{"query": "plombier", "location": "Lyon"}, &created)
	id := created.Job.ID
	for _, tool := range []string{"get_job", "get_job_results", "get_job_errors", "cancel_job", "export_job"} {
		res := call(t, other, tool, map[string]any{"job_id": id}, nil)
		if !res.IsError || !strings.Contains(text(res), "not_found") {
			t.Fatalf("%s leaks another key's job: %s", tool, text(res))
		}
	}
	if _, err := other.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "cleads://jobs/" + id}); err == nil {
		t.Fatal("job resource readable by another key")
	}
	var list struct {
		Jobs []any `json:"jobs"`
	}
	call(t, other, "list_jobs", map[string]any{}, &list)
	if len(list.Jobs) != 0 {
		t.Fatal("list_jobs shows another key's jobs")
	}
	if res := call(t, admin, "get_job", map[string]any{"job_id": id}, nil); res.IsError {
		t.Fatalf("admin cannot see the job: %s", text(res))
	}

	res := call(t, crm, "get_logs", map[string]any{}, nil)
	if !res.IsError || !strings.Contains(text(res), "forbidden") {
		t.Fatalf("get_logs for a non-admin key: %s", text(res))
	}
	var logs backend.LogList
	res = call(t, admin, "get_logs", map[string]any{"local": true, "level": "info", "limit": 200}, &logs)
	if res.IsError || logs.Scope != "process" || structuredSize(res) > 26<<10 {
		t.Fatalf("admin logs: %s", text(res))
	}
	var h struct {
		Health backend.HealthSummary `json:"health"`
	}
	call(t, crm, "get_system_health", map[string]any{}, &h)
	if h.Health.RecentErrors != nil {
		t.Fatal("recent errors exposed to a non-admin key")
	}
}

func TestResourcesAndPrompts(t *testing.T) {
	cs, _ := connect(t, startAPI(t), "")
	ctx := context.Background()
	rr, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "cleads://openapi"})
	if err != nil || !strings.Contains(rr.Contents[0].Text, "openapi: 3") {
		t.Fatalf("openapi: %v", err)
	}
	rr, err = cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "cleads://sectors"})
	if err != nil || !json.Valid([]byte(rr.Contents[0].Text)) {
		t.Fatalf("sectors: %v", err)
	}
	if _, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "cleads://jobs/job_missing"}); err == nil {
		t.Fatal("missing job resource")
	}
	tpl, err := cs.ListResourceTemplates(ctx, nil)
	if err != nil || len(tpl.ResourceTemplates) != 2 {
		t.Fatalf("templates: %v %d", err, len(tpl.ResourceTemplates))
	}
	prompts, err := cs.ListPrompts(ctx, nil)
	if err != nil || len(prompts.Prompts) != 2 {
		t.Fatalf("prompts: %v", err)
	}
	p, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "prospecting_campaign", Arguments: map[string]string{"activity": "plombier", "area": "Lyon"}})
	if err != nil || !strings.Contains(p.Messages[0].Content.(*mcp.TextContent).Text, "create_extraction_job") {
		t.Fatalf("prompt: %v", err)
	}
	p, err = cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "diagnose_job", Arguments: map[string]string{"job_id": "job_x"}})
	if err != nil || !strings.Contains(p.Messages[0].Content.(*mcp.TextContent).Text, "get_job_errors") {
		t.Fatalf("diagnose prompt: %v", err)
	}
}

func TestWaitTimesOut(t *testing.T) {
	srv := startAPI(t)
	cs, _ := connect(t, srv, "")
	var created struct {
		Job struct{ ID string } `json:"job"`
	}
	// A wide area keeps the job busy longer than the 1 s wait.
	call(t, cs, "create_extraction_job", map[string]any{"query": "plombier", "bbox": map[string]any{"min_lat": 45.5, "min_lng": 4.6, "max_lat": 46.0, "max_lng": 5.1}}, &created)
	start := time.Now()
	var w struct {
		TimedOut bool   `json:"timed_out"`
		Reason   string `json:"reason"`
	}
	call(t, cs, "wait_for_job", map[string]any{"job_id": created.Job.ID, "timeout_s": 1}, &w)
	if !w.TimedOut || w.Reason != "timeout" || time.Since(start) > 5*time.Second {
		t.Fatalf("wait did not time out: %+v after %s", w, time.Since(start))
	}
	res := call(t, cs, "wait_for_job", map[string]any{"job_id": created.Job.ID, "timeout_s": 100000}, nil)
	if !res.IsError {
		t.Fatal("timeout above the cap accepted")
	}
	call(t, cs, "cancel_job", map[string]any{"job_id": created.Job.ID}, nil)
}
