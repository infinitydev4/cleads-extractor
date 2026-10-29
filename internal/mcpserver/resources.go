package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cleads/business-extractor/internal/backend"
)

// exportChunkRows bounds one export chunk (also bounded by maxChunkBytes).
const exportChunkRows = 500

func extOf(format string) string {
	if format == "jsonl" {
		return "jsonl"
	}
	return "csv"
}

func mimeOf(format string) string {
	if format == "jsonl" {
		return "application/x-ndjson"
	}
	return "text/csv"
}

func (s *srv) addResources() {
	s.server.AddResource(&mcp.Resource{URI: "cleads://openapi", Name: "openapi", Title: "REST API specification (OpenAPI 3)",
		Description: "Every REST endpoint, parameter and schema of the extractor.", MIMEType: "application/yaml"},
		func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			b, err := s.backendFor(req.Extra)
			if err != nil {
				return nil, err
			}
			spec, err := b.OpenAPI(ctx)
			if err != nil {
				return nil, s.toolError(ctx, "resource openapi", err)
			}
			return textResult(req.Params.URI, "application/yaml", string(spec), nil), nil
		})

	s.server.AddResource(&mcp.Resource{URI: "cleads://sectors", Name: "sectors", Title: "Sector taxonomy",
		Description: "Sector keys mapped to Google types, OSM tags and NAF codes.", MIMEType: "application/json"},
		func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			b, err := s.backendFor(req.Extra)
			if err != nil {
				return nil, err
			}
			secs, err := b.Sectors(ctx)
			if err != nil {
				return nil, s.toolError(ctx, "resource sectors", err)
			}
			js, _ := json.Marshal(secs)
			return textResult(req.Params.URI, "application/json", string(js), nil), nil
		})

	s.server.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: "cleads://jobs/{id}", Name: "job", Title: "Extraction job",
		Description: "Status, specification and counters of one of your jobs.", MIMEType: "application/json"},
		func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			b, err := s.backendFor(req.Extra)
			if err != nil {
				return nil, err
			}
			u, err := url.Parse(req.Params.URI)
			if err != nil {
				return nil, mcp.ResourceNotFoundError(req.Params.URI)
			}
			j, err := b.GetJob(ctx, strings.Trim(u.Path, "/"))
			if err != nil {
				if backend.Code(err) == "not_found" {
					return nil, mcp.ResourceNotFoundError(req.Params.URI)
				}
				return nil, s.toolError(ctx, "resource job", err)
			}
			js, _ := json.Marshal(jobOut{Job: j, Progress: progressOf(j)})
			return textResult(req.Params.URI, "application/json", string(js), nil), nil
		})

	s.server.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: "cleads://jobs/{id}/export/{format}{?cursor,filter}",
		Name: "job-export", Title: "Job export, by chunks",
		Description: fmt.Sprintf("CSV, csv-excel or JSONL export of a job, %d rows (at most ~%d KB) per read. "+
			"The CSV header is in the first chunk only; _meta.next_uri points to the next chunk and _meta.done tells when to stop. "+
			"filter is the URL-encoded query of filters (e.g. useful=true&has_phone=true).", exportChunkRows, maxChunkBytes>>10),
		MIMEType: "text/csv"},
		s.readExport)
}

func (s *srv) backendFor(extra *mcp.RequestExtra) (backend.Backend, error) {
	b, err := s.opts.Backend(extra)
	if err != nil {
		return nil, fmt.Errorf("unauthorized: %w", err)
	}
	return b, nil
}

func (s *srv) readExport(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	b, err := s.backendFor(req.Extra)
	if err != nil {
		return nil, err
	}
	// cleads://jobs/{id}/export/{format}?cursor=..&filter=..
	u, err := url.Parse(req.Params.URI)
	if err != nil {
		return nil, mcp.ResourceNotFoundError(req.Params.URI)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if u.Host != "jobs" || len(parts) != 3 || parts[1] != "export" {
		return nil, mcp.ResourceNotFoundError(req.Params.URI)
	}
	id, format := parts[0], parts[2]
	cursor, _ := strconv.ParseInt(u.Query().Get("cursor"), 10, 64)
	fq, _ := url.ParseQuery(u.Query().Get("filter"))
	f := filtersFrom(fq)
	ch, err := b.ExportChunk(ctx, id, format, f.store(), cursor, exportChunkRows, maxChunkBytes)
	if err != nil {
		if backend.Code(err) == "not_found" {
			return nil, mcp.ResourceNotFoundError(req.Params.URI)
		}
		return nil, s.toolError(ctx, "resource export", err)
	}
	meta := mcp.Meta{"rows": ch.Rows, "done": ch.Done, "cursor": cursor}
	if !ch.Done {
		next, _ := strconv.ParseInt(ch.NextCursor, 10, 64)
		meta["next_uri"] = exportURI(id, format, next, f)
	}
	return textResult(req.Params.URI, mimeOf(format), ch.Text, meta), nil
}

func textResult(uri, mime, text string, meta mcp.Meta) *mcp.ReadResourceResult {
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: mime, Text: text, Meta: meta}}}
}
