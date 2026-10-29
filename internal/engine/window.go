package engine

import (
	"strings"
	"sync"
	"time"

	"github.com/cleads/business-extractor/internal/domain"
)

// windowMinutes is the span of the recent-outcome counters that workers
// publish for the health summary.
const windowMinutes = 5

// outcomeWindow counts source request outcomes per minute, so the health
// summary can report a recent error rate without querying Prometheus.
type outcomeWindow struct {
	mu      sync.Mutex
	buckets map[string]*[windowMinutes + 1]minuteBucket
}

type minuteBucket struct {
	minute int64
	c      domain.OutcomeCounts
}

func newOutcomeWindow() *outcomeWindow {
	return &outcomeWindow{buckets: map[string]*[windowMinutes + 1]minuteBucket{}}
}

// governorName maps a queue to the source whose governor it shares
// (search and details tasks of a source hit the same provider).
func governorName(queue string) string { return strings.TrimSuffix(queue, ".details") }

func (w *outcomeWindow) add(source, outcome string, now time.Time) {
	m := now.Unix() / 60
	w.mu.Lock()
	defer w.mu.Unlock()
	bs := w.buckets[source]
	if bs == nil {
		bs = &[windowMinutes + 1]minuteBucket{}
		w.buckets[source] = bs
	}
	b := &bs[m%int64(len(bs))]
	if b.minute != m {
		*b = minuteBucket{minute: m}
	}
	switch outcome {
	case "ok":
		b.c.OK++
	case "rate_limited":
		b.c.RateLimited++
	case "blocked":
		b.c.Blocked++
	case "permanent":
		b.c.Permanent++
	default:
		b.c.Error++
	}
}

// sum returns the counts of the last windowMinutes full minutes plus the
// current one.
func (w *outcomeWindow) sum(source string, now time.Time) domain.OutcomeCounts {
	m := now.Unix() / 60
	w.mu.Lock()
	defer w.mu.Unlock()
	var out domain.OutcomeCounts
	if bs := w.buckets[source]; bs != nil {
		for _, b := range bs {
			if m-b.minute <= windowMinutes {
				out.Add(b.c)
			}
		}
	}
	return out
}
