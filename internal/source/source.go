// Package source defines the contract every data source implements, plus the
// shared HTTP plumbing (client tuning, errors, retry hints).
package source

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/taxonomy"
)

// Request is one search unit: one query on one geographic cell / page.
type Request struct {
	JobID     string
	Query     string
	Sector    *taxonomy.Sector
	Cell      domain.BBox
	PageToken string
	Page      int
	Language  string
	Country   string
	Location  string
}

// SearchText returns the text to send to full-text engines.
func (r Request) SearchText() string {
	if r.Query != "" {
		return r.Query
	}
	if r.Sector != nil {
		return r.Sector.Label
	}
	return ""
}

// Result is what a source returns for a Request.
type Result struct {
	Businesses []domain.Business
	// NextPageToken, when set, makes the engine enqueue a follow-up task.
	NextPageToken string
	// Saturated means the source returned as many results as it can for one
	// query: the planner splits the cell to find the remaining businesses.
	Saturated bool
}

// Info describes the operational profile of a source.
type Info struct {
	Name string `json:"name"`
	// Description is shown by GET /v1/sources.
	Description string `json:"description"`
	// Official is true for licensed APIs (vs. scraping).
	Official bool `json:"official"`
	// DefaultCellKm is the preferred grid cell size. 0 means the source
	// handles a whole area in one request (no tiling).
	DefaultCellKm float64 `json:"default_cell_km"`
	// MaxCells bounds tiling for this source.
	MaxCells int `json:"max_cells"`
	// RatePerSec / Burst are the default politeness budget per worker.
	RatePerSec float64 `json:"rate_per_sec"`
	Burst      int     `json:"burst"`
	// Concurrency is the max in-flight requests per worker.
	Concurrency int `json:"concurrency"`
	// MinCellKm stops quadtree refinement below this size.
	MinCellKm float64 `json:"min_cell_km"`
	// Timeout bounds one Search call (default 90s).
	Timeout time.Duration `json:"-"`
	// LooseResults: the provider pads results with unrelated nearby places;
	// the engine filters them by activity unless the job opts out.
	LooseResults bool `json:"loose_results"`
}

// Source is implemented by every provider.
type Source interface {
	Info() Info
	Search(ctx context.Context, req Request) (Result, error)
}

// Detailer is implemented by sources that can fetch a full profile for a
// record they returned (e.g. a Google Maps place page). The engine runs
// details as separate tasks so search results stay available immediately.
type Detailer interface {
	Details(ctx context.Context, req DetailsRequest) (domain.Business, error)
}

// DetailsRequest identifies the record to complete.
type DetailsRequest struct {
	DataID   string
	Name     string
	Language string
	Country  string
}

// Error classes understood by the engine.
var (
	// ErrRateLimited: back off this source; retry later.
	ErrRateLimited = errors.New("rate limited")
	// ErrBlocked: the provider is refusing us (captcha, 403). The engine opens
	// the circuit breaker for the source.
	ErrBlocked = errors.New("blocked by provider")
	// ErrPermanent: retrying will not help (bad request, auth).
	ErrPermanent = errors.New("permanent error")
)

// RetryAfterError carries a provider retry hint.
type RetryAfterError struct {
	Err   error
	After time.Duration
}

func (e *RetryAfterError) Error() string { return fmt.Sprintf("%v (retry after %s)", e.Err, e.After) }
func (e *RetryAfterError) Unwrap() error { return e.Err }

// RetryAfter extracts the retry hint from err, if any.
func RetryAfter(err error) time.Duration {
	var ra *RetryAfterError
	if errors.As(err, &ra) {
		return ra.After
	}
	return 0
}

// ClassifyHTTP maps an HTTP status to an error class.
func ClassifyHTTP(resp *http.Response, body []byte) error {
	snippet := string(body)
	if len(snippet) > 300 {
		snippet = snippet[:300]
	}
	base := fmt.Errorf("http %d: %s", resp.StatusCode, snippet)
	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable:
		after := parseRetryAfter(resp.Header.Get("Retry-After"))
		return &RetryAfterError{Err: fmt.Errorf("%w: %v", ErrRateLimited, base), After: after}
	case resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %v", ErrBlocked, base)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusBadRequest ||
		resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnprocessableEntity:
		return fmt.Errorf("%w: %v", ErrPermanent, base)
	case resp.StatusCode >= 500:
		return base
	}
	return base
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return time.Until(t)
	}
	return 0
}

// NewHTTPClient returns a client tuned for many concurrent small requests:
// keep-alive pools sized for the expected concurrency, HTTP/2 when offered,
// strict timeouts so a slow upstream never pins a worker.
// When proxies are given they are used round-robin, one per new connection.
func NewHTTPClient(timeout time.Duration, maxConnsPerHost int, proxies []*url.URL) *http.Client {
	proxyFn := http.ProxyFromEnvironment
	if len(proxies) > 0 {
		var next atomic.Uint64
		proxyFn = func(*http.Request) (*url.URL, error) {
			return proxies[next.Add(1)%uint64(len(proxies))], nil
		}
	}
	tr := &http.Transport{
		Proxy: proxyFn,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   maxConnsPerHost,
		MaxConnsPerHost:       maxConnsPerHost,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

// ParseProxies parses a comma-separated proxy list.
func ParseProxies(list []string) ([]*url.URL, error) {
	out := make([]*url.URL, 0, len(list))
	for _, p := range list {
		if p == "" {
			continue
		}
		u, err := url.Parse(p)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("invalid proxy %q", p)
		}
		out = append(out, u)
	}
	return out, nil
}

// ErrForbiddenAddress is returned when a connection targets a private,
// loopback or link-local address.
var ErrForbiddenAddress = errors.New("destination address is not public")

// NewPublicHTTPClient is NewHTTPClient restricted to public IP addresses. It
// must be used for every URL that comes from third-party data or from API
// clients (websites to crawl, webhooks) to prevent SSRF into the private
// network (Railway internal services, cloud metadata endpoints).
func NewPublicHTTPClient(timeout time.Duration, maxConnsPerHost int) *http.Client {
	c := NewHTTPClient(timeout, maxConnsPerHost, nil)
	tr := c.Transport.(*http.Transport)
	tr.Proxy = nil
	d := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return err
			}
			if !IsPublicIP(ip) {
				return fmt.Errorf("%w: %s", ErrForbiddenAddress, ip)
			}
			return nil
		},
	}
	tr.DialContext = d.DialContext
	return c
}

// IsPublicIP reports whether ip is routable on the public internet.
func IsPublicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT (also used by some PaaS private networks)
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("64:ff9b::/96"),
}
