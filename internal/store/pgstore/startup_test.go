package pgstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const unreachableDSN = "postgres://cleads@127.0.0.1:1/cleads?sslmode=disable" // nothing listens on port 1

func TestOpenWaitGivesUpWithAClearError(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))

	start := time.Now()
	_, err := OpenWait(context.Background(), unreachableDSN, 2, 1500*time.Millisecond, log)
	elapsed := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "gave up after 1.5s") {
		t.Fatalf("want a clear give-up error, got %v", err)
	}
	if elapsed < 1400*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("must keep retrying for about the configured wait, took %s", elapsed)
	}
	if !strings.Contains(logs.String(), "database not ready, retrying") {
		t.Fatalf("each retry must be logged, logs:\n%s", logs.String())
	}
}

func TestOpenWaitWithoutWaitTriesOnce(t *testing.T) {
	var logs bytes.Buffer
	start := time.Now()
	_, err := OpenWait(context.Background(), unreachableDSN, 2, 0, slog.New(slog.NewTextHandler(&logs, nil)))
	if err == nil || !strings.Contains(err.Error(), "1 attempt(s)") {
		t.Fatalf("wait=0 must try exactly once, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("a single attempt must fail fast, took %s", time.Since(start))
	}
}

func TestOpenWaitStopsWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := OpenWait(ctx, unreachableDSN, 2, time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the context error, got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("shutdown during startup must not wait for the full delay, took %s", time.Since(start))
	}
}

// lateProxy listens on a fresh local port only after delay, then relays to
// target: the database "comes up late", as PostgreSQL does after a host boot.
func lateProxy(t *testing.T, target string, delay time.Duration) (addr string) {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr = probe.Addr().String()
	probe.Close()

	done := make(chan struct{})
	var mu sync.Mutex
	var ln net.Listener
	go func() {
		select {
		case <-time.After(delay):
		case <-done:
			return
		}
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return
		}
		mu.Lock()
		ln = l
		mu.Unlock()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				up, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer up.Close()
				go func() { _, _ = io.Copy(up, c) }()
				_, _ = io.Copy(c, up)
			}()
		}
	}()
	t.Cleanup(func() {
		close(done)
		mu.Lock()
		defer mu.Unlock()
		if ln != nil {
			ln.Close()
		}
	})
	return addr
}

func TestOpenWaitSucceedsWhenTheDatabaseComesUpLate(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	target := u.Host
	if u.Port() == "" {
		target = net.JoinHostPort(u.Hostname(), "5432")
	}
	u.Host = lateProxy(t, target, 1500*time.Millisecond)

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	start := time.Now()
	s, err := OpenWait(context.Background(), u.String(), 4, 30*time.Second, log)
	if err != nil {
		t.Fatalf("must succeed once the database answers: %v\nlogs:\n%s", err, logs.String())
	}
	defer s.Close()

	if elapsed := time.Since(start); elapsed < 1200*time.Millisecond {
		t.Fatalf("returned in %s, before the database was reachable", elapsed)
	}
	if err := s.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"database not ready, retrying", "database is ready"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("missing %q in logs:\n%s", want, logs.String())
		}
	}
}
