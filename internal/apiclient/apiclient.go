// Package apiclient implements backend.Backend over the REST API, so the
// stdio MCP command can drive a remote extractor without database access.
// It only ever calls the configured base URL.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cleads/business-extractor/internal/backend"
	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/source"
	"github.com/cleads/business-extractor/internal/store"
	"github.com/cleads/business-extractor/internal/taxonomy"
)

// maxResponse bounds what the client reads from one response.
const maxResponse = 16 << 20

type Client struct {
	base string
	key  string
	hc   *http.Client
	ua   string
}

// New returns a client for the API at baseURL (e.g. https://api.example.com)
// authenticated with apiKey. hc may be nil.
func New(baseURL, apiKey, version string, hc *http.Client) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("CLEADS_URL must be an http(s) URL, got %q", baseURL)
	}
	if apiKey == "" {
		return nil, errors.New("CLEADS_API_KEY is required")
	}
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{base: u.String(), key: apiKey, hc: hc, ua: "cleads-mcp/" + version}, nil
}

// BaseURL is the API base URL (used for download links).
func (c *Client) BaseURL() string { return c.base }

type request struct {
	method string
	path   string
	query  url.Values
	body   any
	header map[string]string
}

// send performs the request and returns the response with its body open
// (the caller closes it), or the API error.
func (c *Client) send(ctx context.Context, r request) (*http.Response, error) {
	var body io.Reader
	if r.body != nil {
		b, err := json.Marshal(r.body)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	u := c.base + r.path
	if len(r.query) > 0 {
		u += "?" + r.query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, r.method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.ua)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range r.header {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		// The URL holds no secret, but say clearly what failed.
		return nil, &backend.Error{Status: http.StatusBadGateway, Code: "unavailable",
			Message: fmt.Sprintf("cannot reach the CLEADS API at %s: %v", c.base, unwrapURLError(err))}
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, decodeError(resp)
	}
	return resp, nil
}

func (c *Client) json(ctx context.Context, r request, out any) error {
	resp, err := c.send(ctx, r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(out); err != nil {
		return fmt.Errorf("decode %s %s: %w", r.method, r.path, err)
	}
	return nil
}

func decodeError(resp *http.Response) error {
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Skipped map[string]string `json:"skipped_sources"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = json.Unmarshal(raw, &body)
	e := &backend.Error{Status: resp.StatusCode, Code: body.Error.Code, Message: body.Error.Message}
	if e.Code == "" {
		e.Code = strings.ToLower(strings.ReplaceAll(http.StatusText(resp.StatusCode), " ", "_"))
		e.Message = "HTTP " + strconv.Itoa(resp.StatusCode)
	}
	if len(body.Skipped) > 0 {
		e.Message += fmt.Sprintf(" (skipped: %v)", body.Skipped)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		e.Message += "; retry in a second"
	}
	return e
}

func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func jobPath(id string, suffix string) string { return "/v1/jobs/" + url.PathEscape(id) + suffix }

// filterQuery encodes f like the REST API expects it.
func filterQuery(f store.BusinessFilter) url.Values {
	q := url.Values{}
	if f.MinCompleteness > 0 {
		q.Set("min_completeness", strconv.Itoa(f.MinCompleteness))
	}
	for k, v := range map[string]bool{"has_phone": f.HasPhone, "has_email": f.HasEmail, "has_website": f.HasWebsite, "useful": f.OnlyUseful} {
		if v {
			q.Set(k, "true")
		}
	}
	for k, v := range map[string]string{"city": f.City, "postal_code": f.PostalPrefix, "category": f.Category, "q": f.Query} {
		if v != "" {
			q.Set(k, v)
		}
	}
	return q
}

// ---------------------------------------------------------------- backend.Backend

func (c *Client) Sources(ctx context.Context) ([]source.Info, error) {
	var out struct {
		Sources []source.Info `json:"sources"`
	}
	err := c.json(ctx, request{method: "GET", path: "/v1/sources"}, &out)
	return out.Sources, err
}

func (c *Client) Sectors(ctx context.Context) ([]taxonomy.Sector, error) {
	var out struct {
		Sectors []taxonomy.Sector `json:"sectors"`
	}
	err := c.json(ctx, request{method: "GET", path: "/v1/sectors"}, &out)
	return out.Sectors, err
}

func (c *Client) OpenAPI(ctx context.Context) ([]byte, error) {
	resp, err := c.send(ctx, request{method: "GET", path: "/openapi.yaml"})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, maxResponse))
}

func (c *Client) CreateJob(ctx context.Context, spec domain.JobSpec, idem string) (backend.CreateJobResult, error) {
	var out backend.CreateJobResult
	r := request{method: "POST", path: "/v1/jobs", body: spec}
	if idem != "" {
		r.header = map[string]string{"Idempotency-Key": idem}
	}
	err := c.json(ctx, r, &out)
	return out, err
}

func (c *Client) GetJob(ctx context.Context, id string) (domain.Job, error) {
	var out struct {
		Job domain.Job `json:"job"`
	}
	err := c.json(ctx, request{method: "GET", path: jobPath(id, "")}, &out)
	return out.Job, err
}

func (c *Client) ListJobs(ctx context.Context, status string, limit int, before time.Time) (backend.JobList, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if status != "" {
		q.Set("status", status)
	}
	if !before.IsZero() {
		q.Set("before", before.Format(time.RFC3339Nano))
	}
	var out backend.JobList
	err := c.json(ctx, request{method: "GET", path: "/v1/jobs", query: q}, &out)
	return out, err
}

func (c *Client) CancelJob(ctx context.Context, id string) (domain.Job, error) {
	var out struct {
		Job domain.Job `json:"job"`
	}
	err := c.json(ctx, request{method: "DELETE", path: jobPath(id, "")}, &out)
	return out.Job, err
}

func (c *Client) JobBusinesses(ctx context.Context, id string, cursor int64, limit int, f store.BusinessFilter) (backend.JobBusinessPage, error) {
	q := filterQuery(f)
	q.Set("cursor", strconv.FormatInt(cursor, 10))
	q.Set("limit", strconv.Itoa(limit))
	var out backend.JobBusinessPage
	err := c.json(ctx, request{method: "GET", path: jobPath(id, "/businesses"), query: q}, &out)
	return out, err
}

func (c *Client) ExportChunk(ctx context.Context, id, format string, f store.BusinessFilter, cursor int64, limit, maxBytes int) (backend.ExportChunk, error) {
	q := filterQuery(f)
	q.Set("cursor", strconv.FormatInt(cursor, 10))
	q.Set("limit", strconv.Itoa(limit))
	q.Set("max_bytes", strconv.Itoa(maxBytes))
	if format != "" {
		q.Set("format", format)
	}
	resp, err := c.send(ctx, request{method: "GET", path: jobPath(id, "/export"), query: q})
	if err != nil {
		return backend.ExportChunk{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)+64<<10))
	if err != nil {
		return backend.ExportChunk{}, err
	}
	rows, _ := strconv.Atoi(resp.Header.Get("X-Export-Rows"))
	done, _ := strconv.ParseBool(resp.Header.Get("X-Export-Done"))
	if format == "" {
		format = "csv"
	}
	return backend.ExportChunk{Format: format, ContentType: resp.Header.Get("Content-Type"), Text: string(b), Rows: rows,
		NextCursor: resp.Header.Get("X-Next-Cursor"), Done: done}, nil
}

func (c *Client) SearchBusinesses(ctx context.Context, cursor int64, limit int, f store.BusinessFilter) (backend.BusinessPage, error) {
	q := filterQuery(f)
	q.Set("cursor", strconv.FormatInt(cursor, 10))
	q.Set("limit", strconv.Itoa(limit))
	var out backend.BusinessPage
	err := c.json(ctx, request{method: "GET", path: "/v1/businesses", query: q}, &out)
	return out, err
}

func (c *Client) GetBusiness(ctx context.Context, id int64) (domain.Business, error) {
	var out domain.Business
	err := c.json(ctx, request{method: "GET", path: "/v1/businesses/" + strconv.FormatInt(id, 10)}, &out)
	return out, err
}

func (c *Client) JobTasks(ctx context.Context, id string, status domain.TaskStatus, limit int) (backend.TaskList, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if status != "" {
		q.Set("status", string(status))
	}
	var out backend.TaskList
	err := c.json(ctx, request{method: "GET", path: jobPath(id, "/tasks"), query: q}, &out)
	return out, err
}

func (c *Client) JobErrors(ctx context.Context, id string) (backend.JobErrors, error) {
	var out backend.JobErrors
	err := c.json(ctx, request{method: "GET", path: jobPath(id, "/errors")}, &out)
	return out, err
}

func (c *Client) Logs(ctx context.Context, lq backend.LogQuery) (backend.LogList, error) {
	q := url.Values{}
	for k, v := range map[string]string{"level": lq.Level, "job_id": lq.JobID, "source": lq.Source} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if !lq.Since.IsZero() {
		q.Set("since", lq.Since.UTC().Format(time.RFC3339Nano))
	}
	if lq.Limit > 0 {
		q.Set("limit", strconv.Itoa(lq.Limit))
	}
	if lq.Local {
		q.Set("local", "true")
	}
	var out backend.LogList
	err := c.json(ctx, request{method: "GET", path: "/v1/logs", query: q}, &out)
	return out, err
}

func (c *Client) Health(ctx context.Context) (backend.HealthSummary, error) {
	var out backend.HealthSummary
	err := c.json(ctx, request{method: "GET", path: "/v1/health/summary"}, &out)
	return out, err
}

func (c *Client) Stats(ctx context.Context) (store.Stats, error) {
	var out store.Stats
	err := c.json(ctx, request{method: "GET", path: "/v1/stats"}, &out)
	return out, err
}

var _ backend.Backend = (*Client)(nil)
