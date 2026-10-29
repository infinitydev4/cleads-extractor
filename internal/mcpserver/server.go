// Package mcpserver exposes the extractor to LLM agents through the Model
// Context Protocol: tools (search, wait, read, export, diagnose), resources
// and prompts. Tools only talk to a backend.Backend, so the same server runs
// in-process behind /mcp (Streamable HTTP) and in the stdio command, which
// calls a remote instance through its REST API.
package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cleads/business-extractor/internal/backend"
	"github.com/cleads/business-extractor/internal/metrics"
	"github.com/cleads/business-extractor/internal/obs"
)

// Output budgets keep tool results to a few tens of KB: an agent's context
// is precious, and anything bigger belongs in an export.
const (
	maxStructuredBytes = 24 << 10 // structured JSON of one tool result
	maxChunkBytes      = 48 << 10 // one export resource chunk
)

type Options struct {
	// Backend returns the backend acting for the caller of a request (one
	// API key). It must fail when the caller cannot be identified.
	Backend func(extra *mcp.RequestExtra) (backend.Backend, error)
	Version string
	// PublicURL is the base URL of the REST API, used to give agents an
	// authenticated download URL for exports. Empty: relative paths.
	PublicURL string
	// MaxWait caps wait_for_job (default 120s).
	MaxWait time.Duration
	// PollEvery is the job polling period of wait_for_job (default 1s).
	PollEvery time.Duration
	Metrics   *metrics.Metrics // optional
	Logger    *slog.Logger
	Redactor  *obs.Redactor
}

func (o *Options) defaults() {
	if o.MaxWait <= 0 {
		o.MaxWait = 120 * time.Second
	}
	if o.PollEvery <= 0 {
		o.PollEvery = time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Redactor == nil {
		o.Redactor = obs.NewRedactor()
	}
}

type srv struct {
	opts   Options
	server *mcp.Server
}

const instructions = `CLEADS Business Extractor: finds businesses by activity and area (Google Maps scraping by default, OpenStreetMap, SIRENE for France), deduplicates them across sources and enriches them (emails, social profiles, SIRET).

Typical flow: list_sectors (optional) -> create_extraction_job -> wait_for_job (repeat while it returns finished=false; results are readable meanwhile) -> get_job_results (compact pages, filters) -> export_job for the full file. Diagnose with get_job_errors, get_system_health (is a provider blocking us?) and get_logs (admin key only).

Business rules: one area shape per job (location, bbox, or center + radius_m). Without country, the country is inferred from the location (pass country when a name is ambiguous). language sets the language of categories and reviews. enrich: true is slower but finds emails. Jobs take from seconds to many minutes; max_results is an exact quota. Unrelated places Google appends are dropped by default (include_unrelated). SIRENE covers France only.`

// New returns an MCP server exposing the extractor.
func New(opts Options) *mcp.Server {
	opts.defaults()
	s := &srv{opts: opts}
	s.server = mcp.NewServer(&mcp.Implementation{Name: "cleads-extractor", Title: "CLEADS Business Extractor", Version: opts.Version},
		&mcp.ServerOptions{Instructions: instructions, Logger: opts.Logger})
	s.addTools()
	s.addResources()
	s.addPrompts()
	return s.server
}

// hints builds tool annotations. Every tool talks to our own API only, so
// none is open-world; tools that create jobs indirectly reach external
// providers, which the descriptions say.
func hints(title string, readOnly, destructive, idempotent bool) *mcp.ToolAnnotations {
	f := false
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: readOnly, DestructiveHint: &destructive, IdempotentHint: idempotent, OpenWorldHint: &f}
}

// handler is a tool body: it returns the structured output and a one-line
// summary for the text content.
type handler[In, Out any] func(ctx context.Context, req *mcp.CallToolRequest, b backend.Backend, in In) (Out, string, error)

// addTool registers a typed tool. Results carry structuredContent plus two
// text blocks: the short summary, then the same JSON for clients that only
// read text (as the specification recommends).
func addTool[In, Out any](s *srv, t *mcp.Tool, h handler[In, Out]) {
	if t.InputSchema == nil {
		t.InputSchema = schemaFor[In](nil)
	}
	if in, ok := t.InputSchema.(*jsonschema.Schema); ok {
		singleTyped(in)
	}
	if t.OutputSchema == nil {
		t.OutputSchema = schemaFor[Out](nil)
	}
	name := t.Name
	mcp.AddTool(s.server, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		var zero Out
		b, err := s.opts.Backend(req.Extra)
		if err == nil {
			var (
				out     Out
				summary string
			)
			out, summary, err = h(ctx, req, b, in)
			if err == nil {
				s.count(name, "ok")
				js, _ := json.Marshal(out)
				content := []mcp.Content{&mcp.TextContent{Text: summary}, &mcp.TextContent{Text: string(js)}}
				if c, ok := any(out).(contentAdder); ok {
					content = append(content, c.content()...)
				}
				return &mcp.CallToolResult{Content: content}, out, nil
			}
		}
		s.count(name, "error")
		return nil, zero, s.toolError(ctx, name, err)
	})
}

// contentAdder lets an output add content blocks (e.g. resource links).
type contentAdder interface{ content() []mcp.Content }

func (s *srv) count(tool, outcome string) {
	if s.opts.Metrics != nil {
		s.opts.Metrics.MCPCalls.WithLabelValues(tool, outcome).Inc()
	}
}

// toolError turns an error into a message for the agent: API errors are
// explained, anything else is reported generically (and logged) so that no
// internal detail or secret reaches the agent.
func (s *srv) toolError(ctx context.Context, tool string, err error) error {
	var be *backend.Error
	switch {
	case errors.As(err, &be):
		return fmt.Errorf("%s: %s", be.Code, s.opts.Redactor.String(be.Message))
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("timeout: the operation took too long, retry")
	case errors.Is(err, context.Canceled):
		return errors.New("cancelled")
	case errors.Is(err, errInput):
		return err
	}
	s.opts.Logger.WarnContext(ctx, "mcp tool failed", "tool", tool, "err", err)
	return errors.New("internal_error: the server could not complete the call; check get_system_health")
}

// errInput marks errors about the tool arguments, returned as is.
var errInput = errors.New("invalid_argument")

func inputErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errInput, fmt.Sprintf(format, args...))
}

func newIdempotencyKey() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "mcp-" + hex.EncodeToString(b[:])
}

// closed returns an empty object schema rejecting every property.
func closed() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{}, AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}}}
}
