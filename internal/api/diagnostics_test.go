package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cleads/business-extractor/internal/backend"
	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/source"
)

// brokenSource always fails permanently, with an error that leaks a proxy
// password unless redaction works.
type brokenSource struct{}

func (brokenSource) Info() source.Info {
	return source.Info{Name: "broken", Description: "always fails", RatePerSec: 100, Burst: 10, Concurrency: 2, MaxCells: 1}
}

func (brokenSource) Search(context.Context, source.Request) (source.Result, error) {
	return source.Result{}, fmt.Errorf("%w: http 500 from https://maps.example/x?id=123456 via http://bob:s3cretpw@proxy:8080", source.ErrPermanent)
}

func waitJob(t *testing.T, url, key, id string) domain.Job {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_, b := do(t, "GET", url+"/v1/jobs/"+id, key, "")
		var r struct {
			Job domain.Job `json:"job"`
		}
		_ = json.Unmarshal(b, &r)
		if r.Job.Status.Terminal() {
			return r.Job
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s not finished", id)
	return domain.Job{}
}

func createJob(t *testing.T, url, key, body string) string {
	t.Helper()
	r, b := do(t, "POST", url+"/v1/jobs", key, body)
	if r.StatusCode != 202 {
		t.Fatalf("create: %d %s", r.StatusCode, b)
	}
	var res backend.CreateJobResult
	_ = json.Unmarshal(b, &res)
	return res.Job.ID
}

func TestDiagnosticsRoutes(t *testing.T) {
	srv, _, stop := setupWith(t, brokenSource{})
	defer stop()
	id := createJob(t, srv.URL, keyA, `{"query":"plombier","location":"Lyon","sources":["broken"]}`)
	j := waitJob(t, srv.URL, keyA, id)
	if j.Status != domain.JobFailed || j.Counters.TasksFailed == 0 {
		t.Fatalf("job should fail: %+v", j)
	}

	// tasks
	r, b := do(t, "GET", srv.URL+"/v1/jobs/"+id+"/tasks?status=failed", keyA, "")
	var tl backend.TaskList
	_ = json.Unmarshal(b, &tl)
	if r.StatusCode != 200 || len(tl.Tasks) == 0 || tl.Tasks[0].Source != "broken" {
		t.Fatalf("tasks: %d %s", r.StatusCode, b)
	}
	if strings.Contains(string(b), "s3cretpw") {
		t.Fatalf("task error leaks a secret: %s", b)
	}
	if r, _ := do(t, "GET", srv.URL+"/v1/jobs/"+id+"/tasks?status=bogus", keyA, ""); r.StatusCode != 400 {
		t.Fatalf("bad status accepted: %d", r.StatusCode)
	}

	// errors, grouped by cause; events arrive asynchronously
	var je backend.JobErrors
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, b = do(t, "GET", srv.URL+"/v1/jobs/"+id+"/errors", keyA, "")
		_ = json.Unmarshal(b, &je)
		if len(je.Groups) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(je.Groups) == 0 || je.Groups[0].Kind != "failed_task" || !strings.Contains(je.Summary, "on broken") ||
		!strings.Contains(je.Groups[0].Cause, "https://maps.example/…") {
		t.Fatalf("errors: %s", b)
	}
	if strings.Contains(string(b), "s3cretpw") {
		t.Fatalf("errors leak a secret: %s", b)
	}

	// isolation: another key sees nothing
	for _, p := range []string{"/tasks", "/errors"} {
		if r, _ := do(t, "GET", srv.URL+"/v1/jobs/"+id+p, keyB, ""); r.StatusCode != 404 {
			t.Fatalf("%s visible to another key: %d", p, r.StatusCode)
		}
	}

	// logs: admin only, from the events table
	if r, _ := do(t, "GET", srv.URL+"/v1/logs", keyA, ""); r.StatusCode != 403 {
		t.Fatalf("logs for non-admin: %d", r.StatusCode)
	}
	var ll backend.LogList
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, b = do(t, "GET", srv.URL+"/v1/logs?level=warn&since=10m&job_id="+id, keyAdmin, "")
		_ = json.Unmarshal(b, &ll)
		if len(ll.Events) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ll.Scope != "database" || len(ll.Events) == 0 || ll.Events[0].JobID != id || ll.Events[0].Worker != "test" {
		t.Fatalf("logs: %s", b)
	}
	if strings.Contains(string(b), "s3cretpw") {
		t.Fatalf("logs leak a secret: %s", b)
	}
	_, b = do(t, "GET", srv.URL+"/v1/logs?local=true&level=info", keyAdmin, "")
	_ = json.Unmarshal(b, &ll)
	if ll.Scope != "process" || len(ll.Events) == 0 {
		t.Fatalf("local logs: %s", b)
	}
	if r, _ := do(t, "GET", srv.URL+"/v1/logs?level=loud", keyAdmin, ""); r.StatusCode != 400 {
		t.Fatalf("bad level: %d", r.StatusCode)
	}

	// health: worker heartbeat aggregated; the broken source is failing
	var h backend.HealthSummary
	_, b = do(t, "GET", srv.URL+"/v1/health/summary", keyA, "")
	_ = json.Unmarshal(b, &h)
	if !h.Database.OK || len(h.Workers) != 1 || h.Status != "degraded" || h.RecentErrors != nil || h.RecentErrorCount == 0 {
		t.Fatalf("health (crm): %s", b)
	}
	verdicts := map[string]string{}
	for _, s := range h.Sources {
		verdicts[s.Name] = s.Verdict
	}
	if verdicts["broken"] != "failing" && verdicts["broken"] != "blocked" {
		t.Fatalf("broken source verdict %q: %s", verdicts["broken"], b)
	}
	if !strings.Contains(h.Summary, "no source is blocked") {
		t.Fatalf("summary: %q", h.Summary)
	}
	_, b = do(t, "GET", srv.URL+"/v1/health/summary", keyAdmin, "")
	_ = json.Unmarshal(b, &h)
	if len(h.RecentErrors) == 0 || strings.Contains(string(b), "s3cretpw") {
		t.Fatalf("health (admin): %s", b)
	}
}

func TestExportChunks(t *testing.T) {
	srv, stop := setup(t)
	defer stop()
	id := createJob(t, srv.URL, keyA, `{"query":"plombier","location":"Lyon"}`)
	waitJob(t, srv.URL, keyA, id)
	r, b := do(t, "GET", srv.URL+"/v1/jobs/"+id+"/export?format=csv&limit=3", keyA, "")
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if r.StatusCode != 200 || len(lines) != 4 || !strings.HasPrefix(lines[0], "id,name,") || r.Header.Get("X-Export-Done") != "false" {
		t.Fatalf("first chunk: %d %v %s", r.StatusCode, r.Header, b)
	}
	next := r.Header.Get("X-Next-Cursor")
	r, b = do(t, "GET", srv.URL+"/v1/jobs/"+id+"/export?format=csv&limit=3&cursor="+next, keyA, "")
	if strings.HasPrefix(string(b), "id,name,") || r.Header.Get("X-Export-Rows") != "3" {
		t.Fatalf("second chunk: %v %s", r.Header, b)
	}
	// byte budget keeps whole rows
	r, b = do(t, "GET", srv.URL+"/v1/jobs/"+id+"/export?format=jsonl&limit=50&max_bytes=1024", keyA, "")
	if n := r.Header.Get("X-Export-Rows"); n == "0" || n == "50" || len(b) > 1024+600 {
		t.Fatalf("byte budget: rows=%s bytes=%d", n, len(b))
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if !json.Valid([]byte(line)) {
			t.Fatalf("truncated row: %q", line)
		}
	}
}
