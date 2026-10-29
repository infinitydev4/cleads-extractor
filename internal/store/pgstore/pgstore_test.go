package pgstore

// Integration tests: they run only when TEST_DATABASE_URL points to a
// disposable PostgreSQL database (tables are truncated).
//
//   TEST_DATABASE_URL=postgres://user@localhost:5432/cleads_test go test ./internal/store/pgstore/

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/cleads/business-extractor/internal/dedup"
	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/engine"
	"github.com/cleads/business-extractor/internal/geocode"
	"github.com/cleads/business-extractor/internal/metrics"
	"github.com/cleads/business-extractor/internal/normalize"
	"github.com/cleads/business-extractor/internal/source"
	"github.com/cleads/business-extractor/internal/source/mock"
	"github.com/cleads/business-extractor/internal/store"
)

func open(t testing.TB) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := Open(context.Background(), dsn, 40, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(context.Background(), `TRUNCATE jobs, tasks, businesses, business_keys, job_businesses RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func newJob(t testing.TB, s *Store, id string, spec domain.JobSpec) domain.Job {
	t.Helper()
	spec.Normalize()
	j := domain.Job{ID: id, Spec: spec, Status: domain.JobRunning, CreatedAt: time.Now()}
	if err := s.CreateJob(context.Background(), &j, nil); err != nil {
		t.Fatal(err)
	}
	return j
}

func biz(i int) domain.Business {
	b := domain.Business{Name: fmt.Sprintf("Entreprise %d", i), PlaceID: fmt.Sprintf("pid%d", i),
		Phone: "0478000000", Lat: 45.76 + float64(i)/1e4, Lng: 4.84, Sources: []string{"mock"}, City: "Lyon"}
	normalize.Business(&b, "FR")
	return b
}

func keysOf(items []domain.Business) [][]string {
	out := make([][]string, len(items))
	for i := range items {
		out[i] = dedup.Keys(&items[i])
	}
	return out
}

func TestMigrationsIdempotent(t *testing.T) {
	s := open(t)
	if err := s.migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentUpsertsCreateEachBusinessOnce(t *testing.T) {
	s := open(t)
	newJob(t, s, "j1", domain.JobSpec{Query: "x", Location: "Lyon"})
	items := make([]domain.Business, 200)
	for i := range items {
		items[i] = biz(i)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			// every goroutine writes the same 200 businesses, in a different order
			batch := make([]domain.Business, len(items))
			for i := range items {
				batch[i] = items[(i+g*25)%len(items)]
			}
			for off := 0; off < len(batch); off += 40 {
				part := batch[off : off+40]
				if _, err := s.UpsertBusinesses(context.Background(), "j1", part, keysOf(part), 0); err != nil {
					errs <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var nb, nk, nl int
	_ = s.pool.QueryRow(context.Background(), `SELECT count(*) FROM businesses`).Scan(&nb)
	_ = s.pool.QueryRow(context.Background(), `SELECT count(*) FROM business_keys WHERE key LIKE 'pid:%'`).Scan(&nk)
	_ = s.pool.QueryRow(context.Background(), `SELECT count(*) FROM job_businesses`).Scan(&nl)
	if nb != 200 || nk != 200 || nl != 200 {
		t.Fatalf("businesses=%d pid keys=%d links=%d, want 200 each", nb, nk, nl)
	}
}

func TestCrossSourceMerge(t *testing.T) {
	s := open(t)
	newJob(t, s, "j", domain.JobSpec{Query: "x", Location: "Lyon"})
	g := domain.Business{Name: "Dupont Plomberie", PlaceID: "ChIJ1", Phone: "+33478000000", Lat: 45.76, Lng: 4.83, Sources: []string{"google_places"}}
	o := domain.Business{Name: "Dupont plomberie", OSMID: "node/9", Phone: "+33478000000", Emails: []string{"contact@dupont.fr"},
		SIRET: "73282932000074", Sources: []string{"osm"}}
	normalize.Business(&g, "FR")
	normalize.Business(&o, "FR")
	r1, err := s.UpsertBusinesses(context.Background(), "j", []domain.Business{g}, keysOf([]domain.Business{g}), 0)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.UpsertBusinesses(context.Background(), "j", []domain.Business{o}, keysOf([]domain.Business{o}), 0)
	if err != nil {
		t.Fatal(err)
	}
	if r1[0].BusinessID != r2[0].BusinessID || !r1[0].NewInJob || r2[0].NewInJob {
		t.Fatalf("not merged: %+v %+v", r1, r2)
	}
	b, _ := s.GetBusiness(context.Background(), r1[0].BusinessID)
	if len(b.Sources) != 2 || len(b.Emails) != 1 || b.SIRET == "" || b.PlaceID == "" || b.OSMID == "" {
		t.Fatalf("merge lost data: %+v", b)
	}
	// the SIRET key now resolves to the same entity
	sir := domain.Business{Name: "SARL DUPONT", SIRET: "73282932000074", Sources: []string{"sirene"}}
	r3, _ := s.UpsertBusinesses(context.Background(), "j", []domain.Business{sir}, keysOf([]domain.Business{sir}), 0)
	if r3[0].BusinessID != r1[0].BusinessID {
		t.Fatal("siret key not linked")
	}
}

func TestQuotaUnderConcurrency(t *testing.T) {
	s := open(t)
	newJob(t, s, "q", domain.JobSpec{Query: "x", Location: "Lyon", MaxResults: 75})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			items := make([]domain.Business, 40)
			for i := range items {
				items[i] = biz(g*1000 + i)
			}
			if _, err := s.UpsertBusinesses(context.Background(), "q", items, keysOf(items), 75); err != nil {
				t.Error(err)
			}
		}(g)
	}
	wg.Wait()
	var n int
	_ = s.pool.QueryRow(context.Background(), `SELECT count(*) FROM job_businesses WHERE job_id = 'q'`).Scan(&n)
	if n != 75 {
		t.Fatalf("linked %d, quota 75", n)
	}
}

func TestQueueLeaseIsExclusive(t *testing.T) {
	s := open(t)
	j := newJob(t, s, "lease", domain.JobSpec{Query: "x", Location: "Lyon"})
	tasks := make([]domain.Task, 500)
	for i := range tasks {
		tasks[i] = domain.Task{Kind: domain.TaskSearch, Source: "mock"}
	}
	err := s.enqueue(context.Background(), j.ID, tasks)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	seen := map[int64]int{}
	var wg sync.WaitGroup
	for w := 0; w < 10; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				got, err := s.LeaseTasks(context.Background(), "mock", 7, time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				if len(got) == 0 {
					return
				}
				mu.Lock()
				for _, tk := range got {
					seen[tk.ID]++
				}
				mu.Unlock()
				for i := range got {
					if err := s.CompleteTask(context.Background(), &got[i], store.TaskOutcome{Done: true}); err != nil {
						t.Error(err)
					}
				}
			}
		}()
	}
	wg.Wait()
	if len(seen) != 500 {
		t.Fatalf("leased %d distinct tasks, want 500", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("task %d leased %d times", id, n)
		}
	}
	got, _ := s.GetJob(context.Background(), j.ID)
	if got.Counters.TasksDone != 500 {
		t.Fatalf("tasks_done = %d", got.Counters.TasksDone)
	}
	fin, _ := s.FinalizeJobs(context.Background())
	if len(fin) != 1 || fin[0].Status != domain.JobCompleted {
		t.Fatalf("finalize: %+v", fin)
	}
}

func TestExpiredLeaseIsReclaimed(t *testing.T) {
	s := open(t)
	j := newJob(t, s, "exp", domain.JobSpec{Query: "x", Location: "Lyon"})
	_ = s.enqueue(context.Background(), j.ID, []domain.Task{{Kind: domain.TaskSearch, Source: "mock"}})
	a, _ := s.LeaseTasks(context.Background(), "mock", 1, 50*time.Millisecond)
	if len(a) != 1 {
		t.Fatal("lease failed")
	}
	time.Sleep(120 * time.Millisecond)
	b, _ := s.LeaseTasks(context.Background(), "mock", 1, time.Minute)
	if len(b) != 1 || b[0].ID != a[0].ID || b[0].Attempts != 2 {
		t.Fatalf("not reclaimed: %+v", b)
	}
	// the first (dead) worker's late completion is fenced off
	if err := s.CompleteTask(context.Background(), &a[0], store.TaskOutcome{Done: true}); err != store.ErrConflict {
		t.Fatalf("stale completion must conflict, got %v", err)
	}
	if err := s.CompleteTask(context.Background(), &b[0], store.TaskOutcome{Done: true}); err != nil {
		t.Fatalf("current lease holder must complete: %v", err)
	}
}

func TestEngineEndToEndOnPostgres(t *testing.T) {
	s := open(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	area := domain.BBox{MinLat: 45.74, MinLng: 4.80, MaxLat: 45.78, MaxLng: 4.86}
	e := engine.New(engine.Config{PollMin: 5 * time.Millisecond, PollMax: 50 * time.Millisecond, FinalizeEvery: 50 * time.Millisecond},
		s, []source.Source{mock.New(mock.Config{CellKm: 1, Pages: 2, DensityKm2: 60})}, geocode.Static{"lyon": area}, nil, metrics.New(), log)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	spec := domain.JobSpec{Query: "plombier", Location: "Lyon"}
	spec.Normalize()
	j := domain.Job{ID: "e2e", Spec: spec, Status: domain.JobQueued, CreatedAt: time.Now()}
	if err := s.CreateJob(context.Background(), &j, []domain.Task{engine.PlanTask(&j)}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		j, _ = s.GetJob(context.Background(), "e2e")
		if j.Status.Terminal() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if j.Status != domain.JobCompleted {
		t.Fatalf("job %+v", j)
	}
	var links int64
	_ = s.pool.QueryRow(context.Background(), `SELECT count(*) FROM job_businesses WHERE job_id = 'e2e'`).Scan(&links)
	if links != j.Counters.Unique || j.Counters.RawFound <= j.Counters.Unique {
		t.Fatalf("counters %+v links %d", j.Counters, links)
	}
	n := 0
	err := s.StreamJobBusinesses(context.Background(), "e2e", store.BusinessFilter{}, func(*store.BusinessRow) error { n++; return nil })
	if err != nil || int64(n) != links {
		t.Fatalf("stream %d rows err %v", n, err)
	}
}

// enqueue adds tasks to an existing job (test helper).
func (s *Store) enqueue(ctx context.Context, jobID string, tasks []domain.Task) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := insertTasks(ctx, tx, jobID, tasks); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET outstanding = outstanding + $2, tasks_total = tasks_total + $2 WHERE id = $1`, jobID, len(tasks)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
