package apiclient_test

import (
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cleads/business-extractor/internal/apiclient"
	"github.com/cleads/business-extractor/internal/backend"
	"github.com/cleads/business-extractor/internal/mcpserver"
	"github.com/cleads/business-extractor/internal/testenv"
)

// session runs the stdio server code (RunStdio) over an in-memory transport
// against a live API, exactly as cmd/mcp does over stdin/stdout.
func session(t *testing.T, url, key string) *mcp.ClientSession {
	t.Helper()
	env := map[string]string{"CLEADS_URL": url, "CLEADS_API_KEY": key, "CLEADS_MCP_MAX_WAIT": "20s"}
	ct, st := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- mcpserver.RunStdio(ctx, "test", func(k string) string { return env[k] }, st, io.Discard)
	}()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close(); cancel(); <-done })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) (bool, string) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	if out != nil && !res.IsError {
		b, _ := json.Marshal(res.StructuredContent)
		_ = json.Unmarshal(b, out)
	}
	return res.IsError, sb.String()
}

func TestStdioToolsOverREST(t *testing.T) {
	srv := testenv.Start(t)
	crm := session(t, srv.URL, testenv.KeyCRM)
	other := session(t, srv.URL, testenv.KeyOther)
	admin := session(t, srv.URL, testenv.KeyAdmin)

	// Same tool list as the HTTP endpoint.
	httpCS, err := mcp.NewClient(&mcp.Implementation{Name: "t"}, nil).Connect(context.Background(),
		&mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", MaxRetries: -1, HTTPClient: bearer(testenv.KeyCRM)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer httpCS.Close()
	if a, b := toolNames(t, crm), toolNames(t, httpCS); !slices.Equal(a, b) || len(a) != 15 {
		t.Fatalf("stdio tools %v != http tools %v", a, b)
	}

	var created struct {
		Job            struct{ ID string } `json:"job"`
		IdempotencyKey string              `json:"idempotency_key"`
	}
	if isErr, txt := call(t, crm, "create_extraction_job", map[string]any{"query": "plombier", "location": "Lyon", "max_results": 40}, &created); isErr {
		t.Fatal(txt)
	}
	id := created.Job.ID
	var again struct {
		Job      struct{ ID string } `json:"job"`
		Replayed bool                `json:"replayed"`
	}
	call(t, crm, "create_extraction_job", map[string]any{"query": "plombier", "location": "Lyon", "max_results": 40, "idempotency_key": created.IdempotencyKey}, &again)
	if again.Job.ID != id || !again.Replayed {
		t.Fatalf("idempotency over REST: %+v", again)
	}
	var w struct {
		Reason string `json:"reason"`
		Job    struct {
			Counters struct{ Unique int } `json:"counters"`
		} `json:"job"`
	}
	if _, txt := call(t, crm, "wait_for_job", map[string]any{"job_id": id, "timeout_s": 20}, &w); w.Reason != "finished" || w.Job.Counters.Unique != 40 {
		t.Fatalf("wait: %s", txt)
	}
	var page struct {
		Items   []map[string]any `json:"items"`
		HasMore bool             `json:"has_more"`
	}
	call(t, crm, "get_job_results", map[string]any{"job_id": id, "limit": 10, "fields": []string{"name", "city"}, "has_phone": true}, &page)
	if len(page.Items) != 10 || len(page.Items[0]) != 2 {
		t.Fatalf("results: %+v", page)
	}

	// export read in chunks through the REST chunk mode
	var exp struct {
		ResourceURI string `json:"resource_uri"`
		DownloadURL string `json:"download_url"`
	}
	call(t, crm, "export_job", map[string]any{"job_id": id, "format": "jsonl"}, &exp)
	if !strings.HasPrefix(exp.DownloadURL, srv.URL+"/v1/jobs/") {
		t.Fatalf("download url %q", exp.DownloadURL)
	}
	rows, uri := 0, exp.ResourceURI
	for i := 0; uri != "" && i < 20; i++ {
		rr, err := crm.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
		if err != nil {
			t.Fatal(err)
		}
		rows += strings.Count(rr.Contents[0].Text, "\n")
		uri, _ = rr.Contents[0].Meta["next_uri"].(string)
	}
	if rows != 40 {
		t.Fatalf("exported %d rows, want 40", rows)
	}

	// isolation, admin-only logs, errors mapped from REST
	if isErr, txt := call(t, other, "get_job", map[string]any{"job_id": id}, nil); !isErr || !strings.Contains(txt, "not_found") {
		t.Fatalf("other key: %s", txt)
	}
	if isErr, txt := call(t, crm, "get_logs", map[string]any{}, nil); !isErr || !strings.Contains(txt, "forbidden") {
		t.Fatalf("crm logs: %s", txt)
	}
	if isErr, txt := call(t, admin, "get_logs", map[string]any{"since": "10m", "local": true, "level": "info"}, nil); isErr {
		t.Fatalf("admin logs: %s", txt)
	}
	for _, tool := range []string{"get_system_health", "get_stats", "list_sources", "list_sectors", "list_jobs"} {
		if isErr, txt := call(t, crm, tool, map[string]any{}, nil); isErr {
			t.Fatalf("%s: %s", tool, txt)
		}
	}
	var je backend.JobErrors
	call(t, crm, "get_job_errors", map[string]any{"job_id": id}, &je)
	if je.JobID != id {
		t.Fatalf("job errors: %+v", je)
	}
	if isErr, txt := call(t, crm, "create_extraction_job", map[string]any{"query": "x"}, nil); !isErr || !strings.Contains(txt, "invalid_spec") {
		t.Fatalf("validation error over REST: %s", txt)
	}
	rr, err := crm.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "cleads://openapi"})
	if err != nil || !strings.Contains(rr.Contents[0].Text, "/v1/logs") {
		t.Fatalf("openapi over REST: %v", err)
	}
}

func TestClientErrors(t *testing.T) {
	if _, err := apiclient.New("ftp://x", "k", "t", nil); err == nil {
		t.Fatal("bad scheme accepted")
	}
	if _, err := apiclient.New("https://x", "", "t", nil); err == nil {
		t.Fatal("empty key accepted")
	}
	srv := testenv.Start(t)
	bad, _ := apiclient.New(srv.URL, "wrong-key-wrong-key-wrong-key", "t", nil)
	if _, err := bad.Stats(context.Background()); backend.Code(err) != "unauthorized" {
		t.Fatalf("bad key: %v", err)
	}
	down, _ := apiclient.New("http://127.0.0.1:1", "some-key-some-key-some-key", "t", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := down.Stats(ctx); backend.Code(err) != "unavailable" || strings.Contains(err.Error(), "some-key") {
		t.Fatalf("unreachable API: %v", err)
	}
}

func toolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, tl := range res.Tools {
		out = append(out, tl.Name)
	}
	slices.Sort(out)
	return out
}
