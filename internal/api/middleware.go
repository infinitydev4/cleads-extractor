package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/cleads/business-extractor/internal/config"
)

type ctxKey int

const (
	ctxKeyID ctxKey = iota
	ctxReqID
)

// keyID returns the authenticated API key id ("" when auth is disabled).
func keyID(r *http.Request) string {
	v, _ := r.Context().Value(ctxKeyID).(string)
	return v
}

func requestID(r *http.Request) string {
	v, _ := r.Context().Value(ctxReqID).(string)
	return v
}

type statusWriter struct {
	http.ResponseWriter
	code  int
	bytes int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach Flush / SetWriteDeadline.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rid := r.Header.Get("X-Request-Id")
		if rid == "" || len(rid) > 64 {
			var b [8]byte
			_, _ = rand.Read(b[:])
			rid = hex.EncodeToString(b[:])
		}
		w.Header().Set("X-Request-Id", rid)
		r = r.WithContext(context.WithValue(r.Context(), ctxReqID, rid))
		sw := &statusWriter{ResponseWriter: w}
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic in handler", "panic", rec, "path", r.URL.Path, "request_id", rid)
				if sw.code == 0 {
					writeError(sw, r, http.StatusInternalServerError, "internal_error", "internal error")
				}
			}
			route := r.Pattern
			if route == "" {
				route = "unmatched"
			}
			d := time.Since(start)
			s.m.HTTPRequests.WithLabelValues(route, strconv.Itoa(sw.code)).Inc()
			s.m.HTTPLatency.WithLabelValues(route).Observe(d.Seconds())
			lvl := slog.LevelInfo
			if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
				lvl = slog.LevelDebug
			}
			s.log.Log(r.Context(), lvl, "http", "method", r.Method, "route", route, "status", sw.code,
				"dur_ms", float64(d.Microseconds())/1000, "bytes", sw.bytes, "request_id", rid, "key", keyID(r))
		}()
		next.ServeHTTP(sw, r)
	})
}

func (s *Server) security(next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range s.cfg.CORSOrigins {
		allowed[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		if o := r.Header.Get("Origin"); o != "" && (allowed[o] || allowed["*"]) {
			h.Set("Access-Control-Allow-Origin", o)
			h.Set("Vary", "Origin")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Key, Idempotency-Key")
			h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			h.Set("Access-Control-Expose-Headers", "X-Request-Id, Location")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// auth accepts "Authorization: Bearer <key>" or "X-API-Key: <key>". Keys
// are compared by SHA-256 digest in constant time.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AuthDisabled {
			next.ServeHTTP(w, r)
			return
		}
		key := r.Header.Get("X-API-Key")
		if key == "" {
			if a := r.Header.Get("Authorization"); len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
				key = strings.TrimSpace(a[7:])
			}
		}
		if key == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="cleads"`)
			writeError(w, r, http.StatusUnauthorized, "unauthorized", "missing API key")
			return
		}
		h := config.KeyHash(key)
		id := ""
		for _, k := range s.cfg.APIKeys {
			if subtle.ConstantTimeCompare(h[:], k.Hash[:]) == 1 {
				id = k.ID
			}
		}
		if id == "" {
			writeError(w, r, http.StatusUnauthorized, "unauthorized", "invalid API key")
			return
		}
		if !s.limiter(id).Allow() {
			w.Header().Set("Retry-After", "1")
			writeError(w, r, http.StatusTooManyRequests, "rate_limited", "too many requests for this API key")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyID, id)))
	})
}

type keyLimiters struct {
	mu   sync.Mutex
	m    map[string]*rate.Limiter
	rate float64
}

func (s *Server) limiter(id string) *rate.Limiter {
	s.limits.mu.Lock()
	defer s.limits.mu.Unlock()
	l, ok := s.limits.m[id]
	if !ok {
		r := s.limits.rate
		l = rate.NewLimiter(rate.Limit(r), max(1, int(r*2)))
		s.limits.m[id] = l
	}
	return l
}
