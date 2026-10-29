package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cleads/business-extractor/internal/backend"
	"github.com/cleads/business-extractor/internal/mcpserver"
)

// mcpHandler serves the MCP Streamable HTTP transport on /mcp. It runs
// behind the same auth middleware as the REST API (same keys, same per-key
// rate limit), so a tool call acts exactly as the REST call it maps to.
//
// The transport is stateless: no session is kept between HTTP requests, so
// any API replica can serve any call (no sticky sessions behind Railway's
// load balancer). Progress notifications still flow on the response stream
// of the call that emits them.
func (s *Server) mcpHandler() http.Handler {
	srv := mcpserver.New(mcpserver.Options{
		Backend:   s.mcpBackend,
		Version:   s.ver,
		PublicURL: s.cfg.PublicURL,
		MaxWait:   s.cfg.MCPMaxWait,
		Metrics:   s.m,
		Logger:    s.log,
		Redactor:  s.redact,
	})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, Logger: s.log, MaxRequestBodyBytes: s.cfg.MaxBodyBytes})
	// The SDK reads the caller's identity from auth.TokenInfo; the key was
	// already verified by s.auth, so the verifier only forwards its id.
	withIdentity := auth.RequireBearerToken(func(_ context.Context, _ string, r *http.Request) (*auth.TokenInfo, error) {
		return &auth.TokenInfo{UserID: keyID(r), Expiration: time.Now().Add(24 * time.Hour)}, nil
	}, nil)(h)
	maxWait := s.cfg.MCPMaxWait
	if maxWait <= 0 {
		maxWait = 120 * time.Second
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Key sent as X-API-Key (or auth disabled): give the SDK middleware a
		// bearer to parse; the identity comes from the request context.
		if a := r.Header.Get("Authorization"); len(a) < 7 || !strings.EqualFold(a[:7], "bearer ") {
			r.Header.Set("Authorization", "Bearer verified")
		}
		// wait_for_job holds the response for up to MCP_MAX_WAIT: lift the
		// server write timeout for this request only, with a bound.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(maxWait + 30*time.Second))
		withIdentity.ServeHTTP(w, r)
	})
}

// mcpBackend maps an MCP request to the in-process backend of its API key.
func (s *Server) mcpBackend(extra *mcp.RequestExtra) (backend.Backend, error) {
	if extra == nil || extra.TokenInfo == nil {
		return nil, errors.New("unauthenticated MCP request")
	}
	return s.Backend(extra.TokenInfo.UserID), nil
}

// checkOrigin rejects browser requests from origins that are not allowed,
// as the MCP specification requires against DNS rebinding: a web page must
// not drive the MCP endpoint through a visitor's browser. Requests without
// Origin (agents, CLIs, server-to-server) are not affected.
func (s *Server) checkOrigin(next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range append(append([]string(nil), s.cfg.MCPAllowedOrigins...), s.cfg.CORSOrigins...) {
		allowed[strings.TrimRight(o, "/")] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o := r.Header.Get("Origin")
		if o == "" || allowed[o] || allowed["*"] || sameOrigin(o, r) {
			next.ServeHTTP(w, r)
			return
		}
		writeError(w, r, http.StatusForbidden, "forbidden_origin", "origin not allowed for /mcp (see MCP_ALLOWED_ORIGINS)")
	})
}

func sameOrigin(origin string, r *http.Request) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil && u.Port() == "" {
		host = h
	}
	return strings.EqualFold(u.Host, host)
}
