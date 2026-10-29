// Package ratelimit protects upstream providers and our own reputation:
// a token bucket whose rate adapts to provider feedback (AIMD), plus a
// circuit breaker that stops all traffic to a provider that blocks us.
package ratelimit

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Governor is safe for concurrent use.
type Governor struct {
	lim      *rate.Limiter
	base     float64
	floor    float64
	mu       sync.Mutex
	open     time.Time // circuit open until
	cooldown time.Duration
	minCool  time.Duration
	maxCool  time.Duration
}

func NewGovernor(perSec float64, burst int) *Governor {
	if perSec <= 0 {
		perSec = 1
	}
	if burst < 1 {
		burst = 1
	}
	return &Governor{
		lim:     rate.NewLimiter(rate.Limit(perSec), burst),
		base:    perSec,
		floor:   perSec / 16,
		minCool: 30 * time.Second,
		maxCool: 30 * time.Minute,
	}
}

// Wait blocks until a request may be sent. It returns ErrOpen immediately
// when the circuit is open, so callers can reschedule instead of sleeping.
func (g *Governor) Wait(ctx context.Context) error {
	if until := g.OpenUntil(); !until.IsZero() {
		return &OpenError{Until: until}
	}
	return g.lim.Wait(ctx)
}

// OpenUntil returns when the circuit closes again, or zero if closed.
func (g *Governor) OpenUntil() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Now().Before(g.open) {
		return g.open
	}
	return time.Time{}
}

// OnSuccess additively restores the rate towards its base value and resets
// the breaker backoff.
func (g *Governor) OnSuccess() {
	g.mu.Lock()
	g.cooldown = 0
	g.mu.Unlock()
	cur := float64(g.lim.Limit())
	if cur < g.base {
		g.lim.SetLimit(rate.Limit(min(g.base, cur+g.base/20)))
	}
}

// OnRateLimited halves the rate (multiplicative decrease).
func (g *Governor) OnRateLimited() {
	cur := float64(g.lim.Limit())
	g.lim.SetLimit(rate.Limit(max(g.floor, cur/2)))
}

// OnBlocked opens the circuit with exponential cooldown.
func (g *Governor) OnBlocked() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cooldown == 0 {
		g.cooldown = g.minCool
	} else {
		g.cooldown = min(g.maxCool, g.cooldown*2)
	}
	g.open = time.Now().Add(g.cooldown)
	g.lim.SetLimit(rate.Limit(max(g.floor, float64(g.lim.Limit())/2)))
	return g.open
}

// Rate returns the current allowed rate.
func (g *Governor) Rate() float64 { return float64(g.lim.Limit()) }

// OpenError is returned by Wait while the circuit is open.
type OpenError struct{ Until time.Time }

func (e *OpenError) Error() string { return "circuit open until " + e.Until.Format(time.RFC3339) }
