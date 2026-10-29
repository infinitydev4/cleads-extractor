package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/cleads/business-extractor/internal/backend"
	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/obs"
	"github.com/cleads/business-extractor/internal/store"
)

const (
	// workerAliveFor: a worker that has not published a heartbeat for this
	// long is considered gone (heartbeats are sent every 10 s by default).
	workerAliveFor = 45 * time.Second
	// recentErrors is the window of "recent_errors" in the health summary.
	recentErrors = 15 * time.Minute
)

// levelsFrom expands a minimum level into the stored level names.
func levelsFrom(min string) ([]string, slog.Level, error) {
	all := []string{"DEBUG", "INFO", "WARN", "ERROR"}
	if min == "" {
		min = "WARN"
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(min)); err != nil {
		return nil, 0, backend.Errorf(http.StatusBadRequest, "invalid_param", "level must be debug, info, warn or error")
	}
	i := slices.Index(all, lvl.String())
	if i < 0 {
		return nil, 0, backend.Errorf(http.StatusBadRequest, "invalid_param", "level must be debug, info, warn or error")
	}
	return all[i:], lvl, nil
}

func trimStar(s string) string { return strings.TrimSuffix(s, "*") }

// ---------------------------------------------------------------- job errors

var (
	reCauseURL    = regexp.MustCompile(`(https?://[^/\s"']+)[^\s"']*`)
	reCauseHex    = regexp.MustCompile(`0x[0-9a-fA-F]+`)
	reCauseNum    = regexp.MustCompile(`\b\d{4,}\b`)
	reCauseQuoted = regexp.MustCompile(`"[^"]{24,}"`)
	reCauseTime   = regexp.MustCompile(`\d{4}-\d\d-\d\dT[\d:.]+(Z|[+-]\d\d:\d\d)?`)
)

// causeOf normalizes an error message so that failures differing only by
// ids, URLs or timestamps fall into the same group.
func causeOf(msg string) string {
	s := reCauseTime.ReplaceAllString(msg, "<time>")
	s = reCauseURL.ReplaceAllString(s, "${1}/…")
	s = reCauseHex.ReplaceAllString(s, "0x…")
	s = reCauseNum.ReplaceAllString(s, "N")
	s = reCauseQuoted.ReplaceAllString(s, `"…"`)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return strings.TrimSpace(s)
}

func groupJobErrors(j domain.Job, failed, pending []domain.Task, events []domain.LogEvent, red *obs.Redactor) backend.JobErrors {
	out := backend.JobErrors{JobID: j.ID, Status: j.Status, JobError: red.String(j.Error), TasksFailed: j.Counters.TasksFailed}
	idx := map[string]int{}
	add := func(kind, src, msg string, at time.Time, task int64) {
		msg = red.String(msg)
		if msg == "" {
			msg = "(no error message)"
		}
		cause := causeOf(msg)
		k := kind + "\x00" + src + "\x00" + cause
		i, ok := idx[k]
		if !ok {
			i = len(out.Groups)
			idx[k] = i
			out.Groups = append(out.Groups, backend.ErrorGroup{Kind: kind, Source: src, Cause: cause, Example: truncateStr(msg, 500)})
		}
		g := &out.Groups[i]
		g.Count++
		if at.After(g.LastSeen) {
			g.LastSeen = at
		}
		if task != 0 && len(g.TaskIDs) < 5 {
			g.TaskIDs = append(g.TaskIDs, task)
		}
	}
	for _, t := range failed {
		add("failed_task", t.Source, t.LastError, time.Time{}, t.ID)
	}
	for _, t := range pending {
		if t.LastError != "" {
			out.TasksRetrying++
			add("retrying_task", t.Source, t.LastError, time.Time{}, t.ID)
		}
	}
	for _, e := range events {
		if e.Msg == "task failed" {
			continue // already counted from the failed task itself
		}
		msg := e.Msg
		if v, ok := e.Attrs["err"]; ok {
			msg += ": " + fmt.Sprint(v)
		} else if v, ok := e.Attrs["error"]; ok && fmt.Sprint(v) != "" {
			msg += ": " + fmt.Sprint(v)
		}
		add("event", e.Source, msg, e.Time, e.TaskID)
	}
	kindRank := map[string]int{"failed_task": 0, "retrying_task": 1, "event": 2}
	slices.SortStableFunc(out.Groups, func(a, b backend.ErrorGroup) int {
		if a.Kind != b.Kind {
			return kindRank[a.Kind] - kindRank[b.Kind]
		}
		return b.Count - a.Count
	})
	if out.Groups == nil {
		out.Groups = []backend.ErrorGroup{}
	}
	out.Summary = errorSummary(out)
	return out
}

func errorSummary(e backend.JobErrors) string {
	if len(e.Groups) == 0 {
		if e.JobError != "" {
			return "job error: " + e.JobError
		}
		return "no error recorded for this job"
	}
	label := map[string]string{"failed_task": "failed", "retrying_task": "retrying", "event": "logged"}
	var parts []string
	for _, g := range e.Groups[:min(4, len(e.Groups))] {
		src := ""
		if g.Source != "" {
			src = " on " + g.Source
		}
		parts = append(parts, fmt.Sprintf("%d × %s%s (%s)", g.Count, g.Cause, src, label[g.Kind]))
	}
	s := strings.Join(parts, "; ")
	if e.JobError != "" {
		s = "job error: " + e.JobError + "; " + s
	}
	return s
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------- health

func (l *local) Health(ctx context.Context) (backend.HealthSummary, error) {
	now := time.Now().UTC()
	h := backend.HealthSummary{CheckedAt: now, RecentWindow: recentErrors.String(), Jobs: map[string]int64{},
		Workers: []backend.WorkerBrief{}, Sources: []backend.SourceHealth{}}
	h.Queue.Pending = map[string]int64{}

	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	start := time.Now()
	err := l.s.store.Ping(pctx)
	cancel()
	h.Database.LatencyMS = float64(time.Since(start).Microseconds()) / 1000
	if err != nil {
		h.Status, h.Summary = "down", "database unreachable: nothing can run"
		h.Database.Error = "unreachable"
		return h, nil
	}
	h.Database.OK = true
	var findings []string
	degraded := false

	st, err := l.s.store.Stats(ctx)
	if err != nil {
		return h, l.fail(ctx, "stats", err)
	}
	h.Queue.Pending, h.Queue.Running, h.Jobs = st.TasksPending, st.TasksRunning, st.Jobs

	workers, err := l.s.store.ListWorkerStatus(ctx, now.Add(-workerAliveFor))
	if err != nil {
		return h, l.fail(ctx, "list workers", err)
	}
	byName := map[string]*backend.SourceHealth{}
	var names []string
	get := func(n string) *backend.SourceHealth {
		if sh, ok := byName[n]; ok {
			return sh
		}
		byName[n] = &backend.SourceHealth{Name: n}
		names = append(names, n)
		return byName[n]
	}
	for _, info := range l.s.eng.Sources() {
		get(info.Name)
	}
	for _, w := range workers {
		h.Workers = append(h.Workers, backend.WorkerBrief{ID: w.ID, Role: w.Role, Version: w.Version, SeenAt: w.SeenAt})
		for _, s := range w.Sources {
			sh := get(s.Name)
			sh.Workers++
			sh.Rate += s.Rate
			sh.BaseRate += s.BaseRate
			sh.InFlight += s.InFlight
			sh.Recent.Add(s.Recent)
			sh.WindowSeconds = max(sh.WindowSeconds, s.WindowSeconds)
			if s.CircuitOpenUntil.After(now) {
				sh.CircuitOpen = true
				if s.CircuitOpenUntil.After(sh.CircuitOpenUntil) {
					sh.CircuitOpenUntil = s.CircuitOpenUntil
				}
			}
		}
	}
	slices.Sort(names)
	if len(workers) == 0 {
		degraded = true
		findings = append(findings, "no live worker: queued tasks will not run (start a ROLE=worker or ROLE=all replica)")
	}
	blocked := []string{}
	for _, n := range names {
		sh := byName[n]
		sh.Pending = st.TasksPending[n] + st.TasksPending[n+".details"]
		judge(sh)
		h.Sources = append(h.Sources, *sh)
		switch sh.Verdict {
		case "blocked":
			blocked = append(blocked, n)
			degraded = true
			findings = append(findings, n+": BLOCKED — "+sh.Detail)
		case "failing", "throttled":
			degraded = true
			findings = append(findings, n+": "+sh.Verdict+" — "+sh.Detail)
		case "no_worker":
			if sh.Pending > 0 {
				degraded = true
			}
			findings = append(findings, n+": "+sh.Detail)
		case "ok":
			findings = append(findings, n+": ok — "+sh.Detail)
		}
	}
	if len(blocked) == 0 {
		findings = append([]string{"no source is blocked"}, findings...)
	}

	evs, err := l.s.store.ListEvents(ctx, store.EventFilter{Levels: []string{"WARN", "ERROR"}, Since: now.Add(-recentErrors), Limit: 200})
	if err != nil {
		return h, l.fail(ctx, "list events", err)
	}
	h.RecentErrorCount = len(evs)
	if l.admin() {
		for _, e := range evs[:min(10, len(evs))] {
			h.RecentErrors = append(h.RecentErrors, e)
		}
	}
	h.Status = "ok"
	if degraded {
		h.Status = "degraded"
	}
	h.Summary = strings.Join(findings, "\n")
	return h, nil
}

// judge sets the verdict of a source from its aggregated worker state.
func judge(sh *backend.SourceHealth) {
	r := sh.Recent
	total := r.Total()
	if total > 0 {
		sh.ErrorRate = float64(total-r.OK) / float64(total)
	}
	win := "5 min"
	if sh.WindowSeconds > 0 {
		win = (time.Duration(sh.WindowSeconds) * time.Second).String()
	}
	counts := fmt.Sprintf("%d requests in %s (%d ok, %d errors, %d rate-limited, %d blocked); rate %.1f/%.1f req/s over %d worker(s)",
		total, win, r.OK, r.Error+r.Permanent, r.RateLimited, r.Blocked, sh.Rate, sh.BaseRate, sh.Workers)
	switch {
	case sh.Workers == 0:
		sh.Verdict = "no_worker"
		sh.Detail = "enabled on the API but no live worker runs it"
		if sh.Pending > 0 {
			sh.Detail += fmt.Sprintf("; %d task(s) waiting", sh.Pending)
		}
	case sh.CircuitOpen:
		sh.Verdict = "blocked"
		sh.Detail = fmt.Sprintf("circuit breaker open until %s (the provider answered 403/captcha); %s",
			sh.CircuitOpenUntil.Format(time.RFC3339), counts)
	case r.Blocked > 0:
		sh.Verdict = "blocked"
		sh.Detail = "blocked recently, circuit closed again; " + counts
	case r.RateLimited > 0 || (sh.BaseRate > 0 && sh.Rate < 0.9*sh.BaseRate):
		sh.Verdict = "throttled"
		sh.Detail = "provider rate limiting (HTTP 429), adaptive rate lowered; " + counts
	case r.Error+r.Permanent > 0 && (r.OK == 0 || (total >= 5 && float64(r.Error+r.Permanent)/float64(total) > 0.2)):
		sh.Verdict = "failing"
		sh.Detail = fmt.Sprintf("%.0f%% of recent requests failed; %s", 100*sh.ErrorRate, counts)
	case total == 0:
		sh.Verdict = "idle"
		sh.Detail = "no request in the recent window"
	default:
		sh.Verdict = "ok"
		sh.Detail = counts
	}
}
