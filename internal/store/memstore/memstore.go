// Package memstore is an in-process Store. It is exact (same semantics as
// pgstore) but single-node: use it for tests, benchmarks of the engine itself
// and local development without PostgreSQL.
package memstore

import (
	"container/heap"
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/store"
)

type link struct {
	seq int64
	biz int64
}

type Store struct {
	mu sync.Mutex

	jobs        map[string]*domain.Job
	outstanding map[string]int // pending+running tasks per job
	notified    map[string]bool

	nextTask int64
	tasks    map[int64]*taskItem
	pending  map[string]*taskHeap // by source
	running  map[int64]time.Time  // lease deadline

	nextBiz  int64
	biz      map[int64]*domain.Business
	keys     map[string]int64
	nextSeq  int64
	jobLinks map[string][]link
	jobSet   map[string]map[int64]struct{}
}

type taskItem struct {
	t     domain.Task
	index int
}

type taskHeap []*taskItem

func (h taskHeap) Len() int { return len(h) }
func (h taskHeap) Less(i, j int) bool {
	a, b := h[i].t, h[j].t
	if !a.RunAfter.Equal(b.RunAfter) {
		return a.RunAfter.Before(b.RunAfter)
	}
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	return a.ID < b.ID
}
func (h taskHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i]; h[i].index = i; h[j].index = j }
func (h *taskHeap) Push(x any)   { it := x.(*taskItem); it.index = len(*h); *h = append(*h, it) }
func (h *taskHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	it.index = -1
	return it
}

func New() *Store {
	return &Store{
		jobs: map[string]*domain.Job{}, outstanding: map[string]int{}, notified: map[string]bool{},
		tasks: map[int64]*taskItem{}, pending: map[string]*taskHeap{}, running: map[int64]time.Time{},
		biz: map[int64]*domain.Business{}, keys: map[string]int64{},
		jobLinks: map[string][]link{}, jobSet: map[string]map[int64]struct{}{},
	}
}

func (s *Store) Ping(context.Context) error { return nil }
func (s *Store) Close()                     {}

func (s *Store) enqueueLocked(t domain.Task) {
	s.nextTask++
	t.ID = s.nextTask
	t.Status = domain.TaskPending
	if t.RunAfter.IsZero() {
		t.RunAfter = time.Now()
	}
	it := &taskItem{t: t}
	s.tasks[t.ID] = it
	h := s.pending[t.Source]
	if h == nil {
		h = &taskHeap{}
		s.pending[t.Source] = h
	}
	heap.Push(h, it)
	s.outstanding[t.JobID]++
	if j := s.jobs[t.JobID]; j != nil {
		j.Counters.TasksTotal++
	}
}

func (s *Store) CreateJob(_ context.Context, job *domain.Job, tasks []domain.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[job.ID]; ok {
		return store.ErrConflict
	}
	j := *job
	s.jobs[j.ID] = &j
	for _, t := range tasks {
		t.JobID = j.ID
		s.enqueueLocked(t)
	}
	job.Counters = j.Counters
	return nil
}

func (s *Store) GetJob(_ context.Context, id string) (domain.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return domain.Job{}, store.ErrNotFound
	}
	return *j, nil
}

func (s *Store) ListJobs(_ context.Context, status string, limit int, before time.Time) ([]domain.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		if status != "" && string(j.Status) != status {
			continue
		}
		if !before.IsZero() && !j.CreatedAt.Before(before) {
			continue
		}
		out = append(out, *j)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].CreatedAt.After(out[b].CreatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *Store) CancelJob(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return store.ErrNotFound
	}
	if j.Status.Terminal() {
		return store.ErrConflict
	}
	j.Status = domain.JobCancelled
	j.FinishedAt = time.Now()
	return nil
}

func (s *Store) MarkJobRunning(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j, ok := s.jobs[id]; ok && j.Status == domain.JobQueued {
		j.Status = domain.JobRunning
		j.StartedAt = time.Now()
	}
	return nil
}

func (s *Store) FailJob(_ context.Context, id, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j, ok := s.jobs[id]; ok && !j.Status.Terminal() {
		j.Status = domain.JobFailed
		j.Error = reason
		j.FinishedAt = time.Now()
	}
	return nil
}

func (s *Store) FinalizeJobs(context.Context) ([]domain.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.Job
	for id, j := range s.jobs {
		if (j.Status == domain.JobRunning || j.Status == domain.JobQueued) && s.outstanding[id] <= 0 {
			j.Status = domain.JobCompleted
			if j.Counters.Unique == 0 && j.Counters.TasksFailed > 0 {
				j.Status = domain.JobFailed
				j.Error = fmt.Sprintf("no result and %d task(s) failed", j.Counters.TasksFailed)
			}
			j.FinishedAt = time.Now()
		}
		if j.Status.Terminal() && !s.notified[id] {
			s.notified[id] = true
			out = append(out, *j)
		}
	}
	return out, nil
}

func (s *Store) LeaseTasks(_ context.Context, src string, n int, lease time.Duration) ([]domain.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	// Requeue expired leases (crashed worker).
	for id, dl := range s.running {
		if dl.Before(now) {
			delete(s.running, id)
			it := s.tasks[id]
			it.t.Status = domain.TaskPending
			it.t.RunAfter = now
			h := s.pending[it.t.Source]
			if h == nil {
				h = &taskHeap{}
				s.pending[it.t.Source] = h
			}
			heap.Push(h, it)
		}
	}
	h := s.pending[src]
	if h == nil {
		return nil, nil
	}
	out := make([]domain.Task, 0, n)
	for h.Len() > 0 && len(out) < n {
		top := (*h)[0]
		if top.t.RunAfter.After(now) {
			break
		}
		heap.Pop(h)
		j := s.jobs[top.t.JobID]
		if j == nil || j.Status.Terminal() {
			// cancelled or finished job: drop the task
			delete(s.tasks, top.t.ID)
			s.outstanding[top.t.JobID]--
			continue
		}
		top.t.Status = domain.TaskRunning
		top.t.Attempts++
		s.running[top.t.ID] = now.Add(lease)
		out = append(out, top.t)
	}
	return out, nil
}

func (s *Store) ExtendLease(_ context.Context, ids []int64, lease time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dl := time.Now().Add(lease)
	for _, id := range ids {
		if _, ok := s.running[id]; ok {
			s.running[id] = dl
		}
	}
	return nil
}

func (s *Store) CompleteTask(_ context.Context, t *domain.Task, out store.TaskOutcome) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.tasks[t.ID]
	if !ok {
		return store.ErrNotFound
	}
	if _, running := s.running[t.ID]; !running || it.t.Attempts != t.Attempts {
		return store.ErrConflict // lease lost (fencing)
	}
	delete(s.running, t.ID)
	j := s.jobs[t.JobID]
	if j != nil {
		addCounters(&j.Counters, out.Delta)
	}
	switch {
	case out.Done:
		delete(s.tasks, t.ID)
		s.outstanding[t.JobID]--
		if j != nil {
			j.Counters.TasksDone++
		}
	case !out.RetryAt.IsZero():
		it.t.Status = domain.TaskPending
		it.t.RunAfter = out.RetryAt
		it.t.LastError = out.Err
		if out.KeepAttempt {
			it.t.Attempts--
		}
		heap.Push(s.pending[it.t.Source], it)
	default:
		delete(s.tasks, t.ID)
		s.outstanding[t.JobID]--
		if j != nil {
			j.Counters.TasksFailed++
		}
	}
	if j != nil && !j.Status.Terminal() {
		for _, nt := range out.NewTasks {
			nt.JobID = t.JobID
			s.enqueueLocked(nt)
		}
		if j.Spec.MaxResults > 0 && j.Counters.Unique >= int64(j.Spec.MaxResults) {
			j.Status = domain.JobCompleted
			j.FinishedAt = time.Now()
		}
	}
	return nil
}

func addCounters(dst *domain.Counters, d domain.Counters) {
	dst.RawFound += d.RawFound
	dst.Unique += d.Unique
	dst.Complete += d.Complete
	dst.CRMUseful += d.CRMUseful
	dst.Enriched += d.Enriched
}

func (s *Store) UpsertBusinesses(_ context.Context, jobID string, items []domain.Business, keys [][]string, maxInJob int) ([]store.UpsertItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	res := make([]store.UpsertItem, len(items))
	set := s.jobSet[jobID]
	if set == nil {
		set = map[int64]struct{}{}
		s.jobSet[jobID] = set
	}
	for i := range items {
		var id int64
		for _, k := range keys[i] {
			if v, ok := s.keys[k]; ok {
				id = v
				break
			}
		}
		var b *domain.Business
		if id == 0 {
			s.nextBiz++
			id = s.nextBiz
			cp := items[i]
			cp.ID = id
			cp.FirstSeenAt = now
			cp.LastSeenAt = now
			cp.ComputeCompleteness()
			if len(keys[i]) > 0 {
				cp.DedupKey = keys[i][0]
			}
			b = &cp
			s.biz[id] = b
		} else {
			b = s.biz[id]
			b.Merge(&items[i])
			b.LastSeenAt = now
		}
		for _, k := range keys[i] {
			if _, ok := s.keys[k]; !ok {
				s.keys[k] = id
			}
		}
		_, inJob := set[id]
		if !inJob && maxInJob > 0 && len(set) >= maxInJob {
			res[i] = store.UpsertItem{BusinessID: id}
			continue
		}
		if !inJob {
			set[id] = struct{}{}
			s.nextSeq++
			s.jobLinks[jobID] = append(s.jobLinks[jobID], link{seq: s.nextSeq, biz: id})
		}
		res[i] = store.UpsertItem{
			BusinessID: id, NewInJob: !inJob, Complete: b.IsComplete(), CRMUseful: b.IsCRMUseful(),
			NeedsEnrich: b.Website != "" && b.EnrichedAt.IsZero(), Website: b.Website,
			NeedsDetails: b.DataID != "" && b.DetailsAt.IsZero(), DataID: b.DataID,
		}
	}
	return res, nil
}

func (s *Store) ApplyPatch(_ context.Context, jobID string, id int64, patch *domain.Business) (bool, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.biz[id]
	if !ok {
		return false, false, store.ErrNotFound
	}
	wasC, wasU := b.IsComplete(), b.IsCRMUseful()
	b.Merge(patch)
	if patch.SIRET != "" {
		if _, exists := s.keys["siret:"+patch.SIRET]; !exists {
			s.keys["siret:"+patch.SIRET] = id
		}
	}
	return !wasC && b.IsComplete(), !wasU && b.IsCRMUseful(), nil
}

func match(b *domain.Business, f store.BusinessFilter) bool {
	if b.Completeness < f.MinCompleteness {
		return false
	}
	if f.HasPhone && b.Phone == "" || f.HasEmail && len(b.Emails) == 0 || f.HasWebsite && b.Website == "" {
		return false
	}
	if f.OnlyUseful && !b.IsCRMUseful() {
		return false
	}
	if f.City != "" && !strings.EqualFold(b.City, f.City) {
		return false
	}
	if f.PostalPrefix != "" && !strings.HasPrefix(b.PostalCode, f.PostalPrefix) {
		return false
	}
	if f.Category != "" && !strings.Contains(strings.ToLower(b.Category), strings.ToLower(f.Category)) {
		return false
	}
	if f.Query != "" && !strings.Contains(strings.ToLower(b.Name), strings.ToLower(f.Query)) {
		return false
	}
	return true
}

func (s *Store) ListJobBusinesses(_ context.Context, jobID string, afterSeq int64, limit int, f store.BusinessFilter) ([]store.BusinessRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	links := s.jobLinks[jobID]
	i := sort.Search(len(links), func(i int) bool { return links[i].seq > afterSeq })
	out := make([]store.BusinessRow, 0, min(limit, len(links)-i))
	for ; i < len(links) && len(out) < limit; i++ {
		b := s.biz[links[i].biz]
		if match(b, f) {
			out = append(out, store.BusinessRow{Seq: links[i].seq, Business: *b})
		}
	}
	return out, nil
}

func (s *Store) StreamJobBusinesses(ctx context.Context, jobID string, f store.BusinessFilter, fn func(*store.BusinessRow) error) error {
	var after int64
	for {
		rows, err := s.ListJobBusinesses(ctx, jobID, after, 1000, f)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			// ListJobBusinesses scans until it fills the page: empty means done.
			return nil
		}
		for i := range rows {
			if err := fn(&rows[i]); err != nil {
				return err
			}
		}
		after = rows[len(rows)-1].Seq
	}
}

func (s *Store) SearchBusinesses(_ context.Context, afterID int64, limit int, f store.BusinessFilter) ([]domain.Business, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.Business, 0, limit)
	for id := afterID + 1; id <= s.nextBiz && len(out) < limit; id++ {
		if b, ok := s.biz[id]; ok && match(b, f) {
			out = append(out, *b)
		}
	}
	return out, nil
}

func (s *Store) GetBusiness(_ context.Context, id int64) (domain.Business, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.biz[id]
	if !ok {
		return domain.Business{}, store.ErrNotFound
	}
	return *b, nil
}

func (s *Store) Stats(context.Context) (store.Stats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := store.Stats{Businesses: int64(len(s.biz)), Jobs: map[string]int64{}, TasksPending: map[string]int64{}}
	for _, j := range s.jobs {
		st.Jobs[string(j.Status)]++
	}
	for src, h := range s.pending {
		st.TasksPending[src] = int64(h.Len())
	}
	st.TasksRunning = int64(len(s.running))
	return st, nil
}
