package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cleads/business-extractor/internal/testenv"
)

// TestStdioBinary builds the command and talks to it over stdin/stdout, as
// Claude Desktop does.
func TestStdioBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := filepath.Join(t.TempDir(), "cleads-mcp")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	srv := testenv.Start(t)
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "CLEADS_URL="+srv.URL, "CLEADS_API_KEY="+testenv.KeyCRM)
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect: %v (stderr: %s)", err, stderr.String())
	}
	tools, err := cs.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 15 {
		t.Fatalf("tools: %v %d", err, len(tools.Tools))
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_system_health", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("health: %v %+v", err, res)
	}
	_ = cs.Close() // stops the process, so stderr is complete
	if strings.Contains(stderr.String(), testenv.KeyCRM) {
		t.Fatal("the API key appears on stderr")
	}
	if !strings.Contains(stderr.String(), "connected") {
		t.Fatalf("stderr: %s", stderr.String())
	}
}

// lockedBuffer is written by the stderr copier while the test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestStdioRequiresConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := filepath.Join(t.TempDir(), "cleads-mcp")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin)
	cmd.Env = []string{"CLEADS_URL=https://x.example"}
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "CLEADS_API_KEY is required") {
		t.Fatalf("missing key: %v %s", err, out)
	}
}
