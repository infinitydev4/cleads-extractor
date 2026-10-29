package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestHealthcheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			http.Error(w, "down", http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	env := func(k string) string {
		if k == "PORT" {
			return u.Port()
		}
		return ""
	}
	if code := healthcheck(nil, env, io.Discard); code != 0 {
		t.Fatalf("healthy server: exit %d", code)
	}
	if code := healthcheck([]string{srv.URL + "/readyz"}, env, io.Discard); code != 1 {
		t.Fatalf("503 must fail, got exit %d", code)
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	cu, _ := url.Parse(closed.URL)
	closed.Close()
	if code := healthcheck(nil, func(string) string { return cu.Port() }, io.Discard); code != 1 {
		t.Fatalf("unreachable server must fail, got exit %d", code)
	}
}
