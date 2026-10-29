package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type headerRT struct {
	h  http.Header
	rt http.RoundTripper
}

func (h headerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h.h {
		r.Header[k] = v
	}
	return h.rt.RoundTrip(r)
}

// mcpSession connects to /mcp over Streamable HTTP with the given headers.
func mcpSession(t *testing.T, url string, hdr ...string) (*mcp.ClientSession, error) {
	t.Helper()
	h := http.Header{}
	for i := 0; i+1 < len(hdr); i += 2 {
		h.Set(hdr[i], hdr[i+1])
	}
	tr := &mcp.StreamableClientTransport{Endpoint: url + "/mcp", MaxRetries: -1,
		HTTPClient: &http.Client{Transport: headerRT{h: h, rt: http.DefaultTransport}}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(context.Background(), tr, nil)
	if err == nil {
		t.Cleanup(func() { _ = cs.Close() })
	}
	return cs, err
}

func mcpCall(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, string) {
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
	return res, sb.String()
}

func TestMCPOverHTTP(t *testing.T) {
	srv, _, stop := setupWith(t)
	defer stop()

	// Same authentication as REST.
	if _, err := mcpSession(t, srv.URL); err == nil {
		t.Fatal("MCP session opened without an API key")
	}
	if r, _ := do(t, "POST", srv.URL+"/mcp", "", `{}`); r.StatusCode != 401 {
		t.Fatalf("no key: %d", r.StatusCode)
	}
	// Origin validation (DNS rebinding protection).
	r, _ := do(t, "POST", srv.URL+"/mcp", keyA, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "Origin", "https://evil.example",
		"Content-Type", "application/json", "Accept", "application/json, text/event-stream")
	if r.StatusCode != 403 {
		t.Fatalf("foreign origin: %d", r.StatusCode)
	}

	crm, err := mcpSession(t, srv.URL, "Authorization", "Bearer "+keyA)
	if err != nil {
		t.Fatal(err)
	}
	other, err := mcpSession(t, srv.URL, "X-API-Key", keyB) // the X-API-Key header works too
	if err != nil {
		t.Fatal(err)
	}
	admin, err := mcpSession(t, srv.URL, "Authorization", "Bearer "+keyAdmin)
	if err != nil {
		t.Fatal(err)
	}

	res, txt := mcpCall(t, crm, "create_extraction_job", map[string]any{"query": "plombier", "location": "Lyon"})
	if res.IsError {
		t.Fatal(txt)
	}
	var created struct {
		Job struct{ ID string } `json:"job"`
	}
	b, _ := json.Marshal(res.StructuredContent)
	_ = json.Unmarshal(b, &created)
	id := created.Job.ID

	// The job belongs to the key that created it, over MCP as over REST.
	if r, _ := do(t, "GET", srv.URL+"/v1/jobs/"+id, keyA, ""); r.StatusCode != 200 {
		t.Fatalf("REST does not see the MCP job: %d", r.StatusCode)
	}
	if res, txt := mcpCall(t, other, "get_job", map[string]any{"job_id": id}); !res.IsError || !strings.Contains(txt, "not_found") {
		t.Fatalf("other key sees the job: %s", txt)
	}
	if res, txt := mcpCall(t, admin, "get_job", map[string]any{"job_id": id}); res.IsError {
		t.Fatalf("admin: %s", txt)
	}
	if res, txt := mcpCall(t, crm, "wait_for_job", map[string]any{"job_id": id, "timeout_s": 10}); res.IsError || !strings.Contains(txt, "finished") {
		t.Fatalf("wait: %s", txt)
	}
	if res, txt := mcpCall(t, crm, "get_logs", map[string]any{}); !res.IsError || !strings.Contains(txt, "forbidden") {
		t.Fatalf("logs for crm: %s", txt)
	}
	if res, txt := mcpCall(t, admin, "get_logs", map[string]any{"since": "1h"}); res.IsError {
		t.Fatalf("logs for admin: %s", txt)
	}
	// A secret never comes back in a tool result.
	if _, txt := mcpCall(t, admin, "get_system_health", nil); strings.Contains(txt, keyA) || strings.Contains(txt, keyAdmin) {
		t.Fatal("health leaks a key")
	}
}
