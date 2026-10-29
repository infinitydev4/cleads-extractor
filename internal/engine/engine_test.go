package engine

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/geo"
	"github.com/cleads/business-extractor/internal/geocode"
	"github.com/cleads/business-extractor/internal/metrics"
	"github.com/cleads/business-extractor/internal/source"
	"github.com/cleads/business-extractor/internal/source/mock"
	"github.com/cleads/business-extractor/internal/store"
	"github.com/cleads/business-extractor/internal/store/memstore"
)

var lyon = domain.BBox{MinLat: 45.74, MinLng: 4.80, MaxLat: 45.78, MaxLng: 4.86}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func startEngine(t *testing.T, st store.Store, srcs ...source.Source) (*Engine, func()) {
	t.Helper()
	gc := geocode.Static{"lyon": lyon}
	e := New(Config{PollMin: 5 * time.Millisecond, PollMax: 20 * time.Millisecond, FinalizeEvery: 20 * time.Millisecond,
		Lease: 5 * time.Second, DrainTimeout: 2 * time.Second, MinRetryAfter: 10 * time.Millisecond}, st, srcs, gc, nil, metrics.New(), quietLog())
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = e.Run(ctx) }()
	return e, func() { cancel(); wg.Wait() }
}

func submit(t *testing.T, st store.Store, spec domain.JobSpec) domain.Job {
	t.Helper()
	spec.Normalize()
	if err := spec.Validate(0); err != nil {
		t.Fatal(err)
	}
	j := domain.Job{ID: "job_" + t.Name(), Spec: spec, Status: domain.JobQueued, CreatedAt: time.Now()}
	if err := st.CreateJob(context.Background(), &j, []domain.Task{PlanTask(&j)}); err != nil {
		t.Fatal(err)
	}
	return j
}

func waitDone(t *testing.T, st store.Store, id string, timeout time.Duration) domain.Job {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		j, err := st.GetJob(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if j.Status.Terminal() {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	j, _ := st.GetJob(context.Background(), id)
	t.Fatalf("job not finished after %s: %+v", timeout, j)
	return j
}

func TestEndToEndDedupAndRefinement(t *testing.T) {
	st := memstore.New()
	src := mock.New(mock.Config{CellKm: 1, DensityKm2: 60, Pages: 2})
	_, stop := startEngine(t, st, src)
	defer stop()

	j := submit(t, st, domain.JobSpec{Query: "plombier", Location: "Lyon"})
	j = waitDone(t, st, j.ID, 20*time.Second)
	c := j.Counters
	if j.Status != domain.JobCompleted {
		t.Fatalf("status %s: %s", j.Status, j.Error)
	}
	if c.Unique == 0 || c.RawFound <= c.Unique {
		t.Fatalf("overlapping cells must produce duplicates: %+v", c)
	}
	if c.CRMUseful == 0 || c.CRMUseful > c.Unique || c.Complete > c.Unique {
		t.Fatalf("inconsistent counters %+v", c)
	}
	// 60 businesses/km² with 2 pages of 20 per query saturates 1 km cells:
	// the planner must have split some of them.
	if c.TasksTotal <= 1+gridSize(t) {
		t.Fatalf("expected quadtree refinement, tasks_total=%d grid=%d", c.TasksTotal, gridSize(t))
	}
	// listing returns exactly the unique set, ordered by seq, no duplicates
	rows, _ := st.ListJobBusinesses(context.Background(), j.ID, 0, 100_000, store.BusinessFilter{})
	if int64(len(rows)) != c.Unique {
		t.Fatalf("listing %d rows, counter says %d", len(rows), c.Unique)
	}
	seen := map[int64]bool{}
	for i, r := range rows {
		if seen[r.ID] || (i > 0 && r.Seq <= rows[i-1].Seq) {
			t.Fatal("duplicate or unordered row")
		}
		seen[r.ID] = true
		if r.Phone != "" && r.Phone[0] != '+' {
			t.Fatalf("phone not normalized: %q", r.Phone)
		}
	}
}

func gridSize(t *testing.T) int64 {
	t.Helper()
	return int64(len(geo.Grid(lyon, 1, 0)))
}

func TestMaxResultsStopsJob(t *testing.T) {
	st := memstore.New()
	_, stop := startEngine(t, st, mock.New(mock.Config{CellKm: 0.5}))
	defer stop()
	j := submit(t, st, domain.JobSpec{Query: "plombier", Location: "Lyon", MaxResults: 50})
	j = waitDone(t, st, j.ID, 10*time.Second)
	if j.Status != domain.JobCompleted || j.Counters.Unique != 50 {
		t.Fatalf("job %+v", j)
	}
}

func TestGeocodeFailureFailsJob(t *testing.T) {
	st := memstore.New()
	_, stop := startEngine(t, st, mock.New(mock.Config{}))
	defer stop()
	j := submit(t, st, domain.JobSpec{Query: "plombier", Location: "Atlantis"})
	j = waitDone(t, st, j.ID, 5*time.Second)
	if j.Status != domain.JobFailed || j.Error == "" {
		t.Fatalf("job %+v", j)
	}
}

// flaky fails the first N calls with the given error.
type flaky struct {
	inner source.Source
	err   error
	fails atomic.Int32
	calls atomic.Int32
}

func (f *flaky) Info() source.Info {
	i := f.inner.Info()
	i.Name = "flaky"
	return i
}

func (f *flaky) Search(ctx context.Context, r source.Request) (source.Result, error) {
	f.calls.Add(1)
	if f.fails.Add(-1) >= 0 {
		return source.Result{}, f.err
	}
	return f.inner.Search(ctx, r)
}

func TestRateLimitedTasksAreRetriedNotFailed(t *testing.T) {
	st := memstore.New()
	f := &flaky{inner: mock.New(mock.Config{CellKm: 5}), err: &source.RetryAfterError{Err: source.ErrRateLimited, After: 10 * time.Millisecond}}
	f.fails.Store(3)
	_, stop := startEngine(t, st, f)
	defer stop()
	j := submit(t, st, domain.JobSpec{Query: "x", BBox: &domain.BBox{MinLat: 45.74, MinLng: 4.80, MaxLat: 45.75, MaxLng: 4.81}})
	j = waitDone(t, st, j.ID, 30*time.Second)
	if j.Status != domain.JobCompleted || j.Counters.TasksFailed != 0 || f.calls.Load() < 4 {
		t.Fatalf("job %+v calls %d", j, f.calls.Load())
	}
}

func TestPermanentErrorFailsTask(t *testing.T) {
	st := memstore.New()
	f := &flaky{inner: mock.New(mock.Config{CellKm: 50}), err: source.ErrPermanent}
	f.fails.Store(1000)
	_, stop := startEngine(t, st, f)
	defer stop()
	j := submit(t, st, domain.JobSpec{Query: "x", BBox: &lyon})
	j = waitDone(t, st, j.ID, 5*time.Second)
	if j.Status != domain.JobFailed || j.Counters.TasksFailed != 1 || f.calls.Load() != 1 {
		t.Fatalf("job %+v calls %d", j, f.calls.Load())
	}
}

func TestCancel(t *testing.T) {
	st := memstore.New()
	_, stop := startEngine(t, st, mock.New(mock.Config{CellKm: 0.2, Latency: 20 * time.Millisecond, Concurrency: 2}))
	defer stop()
	j := submit(t, st, domain.JobSpec{Query: "x", BBox: &lyon})
	time.Sleep(100 * time.Millisecond)
	if err := st.CancelJob(context.Background(), j.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	j, _ = st.GetJob(context.Background(), j.ID)
	before := j.Counters.TasksDone
	time.Sleep(300 * time.Millisecond)
	j, _ = st.GetJob(context.Background(), j.ID)
	if j.Status != domain.JobCancelled || j.Counters.TasksDone > before+2 {
		t.Fatalf("cancelled job kept running: %+v (before %d)", j.Counters, before)
	}
}

func TestSelectSources(t *testing.T) {
	e := New(Config{}, memstore.New(), []source.Source{mock.New(mock.Config{})}, nil, nil, metrics.New(), quietLog())
	names, skipped := e.SelectSources(&domain.JobSpec{Query: "plombier", Sources: []string{"mock", "sirene"}, Country: "FR"})
	if len(names) != 1 || names[0] != "mock" || skipped["sirene"] == "" {
		t.Fatalf("names %v skipped %v", names, skipped)
	}
}

// detailing wraps the mock source with a Detailer that adds a description
// and a phone, and gives every result a feature id.
type detailing struct {
	inner source.Source
	calls atomic.Int32
}

func (d *detailing) Info() source.Info { i := d.inner.Info(); i.Name = "maps"; return i }

func (d *detailing) Search(ctx context.Context, r source.Request) (source.Result, error) {
	res, err := d.inner.Search(ctx, r)
	for i := range res.Businesses {
		res.Businesses[i].DataID = "0x1:" + res.Businesses[i].PlaceID
		res.Businesses[i].Phone = ""
	}
	return res, err
}

func (d *detailing) Details(_ context.Context, r source.DetailsRequest) (domain.Business, error) {
	d.calls.Add(1)
	return domain.Business{Phone: "0478000000", Details: &domain.Details{Description: "desc " + r.DataID}, DetailsAt: time.Now()}, nil
}

func TestDetailsAreFetchedOncePerBusiness(t *testing.T) {
	st := memstore.New()
	src := &detailing{inner: mock.New(mock.Config{CellKm: 1, PhoneRatio: 0.0001})}
	_, stop := startEngine(t, st, src)
	defer stop()
	j := submit(t, st, domain.JobSpec{Query: "x", BBox: &domain.BBox{MinLat: 45.74, MinLng: 4.80, MaxLat: 45.76, MaxLng: 4.83}})
	j = waitDone(t, st, j.ID, 20*time.Second)
	if j.Status != domain.JobCompleted || j.Counters.Unique == 0 {
		t.Fatalf("job %+v", j)
	}
	if int64(src.calls.Load()) != j.Counters.Unique {
		t.Fatalf("details calls %d, unique %d (want exactly one per business)", src.calls.Load(), j.Counters.Unique)
	}
	rows, _ := st.ListJobBusinesses(context.Background(), j.ID, 0, 100000, store.BusinessFilter{})
	for _, r := range rows {
		if r.Details == nil || r.Phone != "+33478000000" || r.DetailsAt.IsZero() {
			t.Fatalf("details not merged: %+v", r.Business)
		}
	}
	// the phone came from details: useful counter must reflect it
	if j.Counters.CRMUseful < j.Counters.Unique*9/10 {
		t.Fatalf("crm_useful %d of %d after details", j.Counters.CRMUseful, j.Counters.Unique)
	}
}

type loose struct{ source.Source }

func (l loose) Info() source.Info {
	i := l.Source.Info()
	i.Name = "loose"
	i.LooseResults = true
	return i
}

func TestUnrelatedResultsAreDropped(t *testing.T) {
	area := &domain.BBox{MinLat: 45.74, MinLng: 4.80, MaxLat: 45.75, MaxLng: 4.81}
	for _, tc := range []struct {
		query string
		all   bool
		want  bool // results expected
	}{{"plombier", false, true}, {"coiffeur", false, false}, {"coiffeur", true, true}} {
		st := memstore.New()
		_, stop := startEngine(t, st, loose{mock.New(mock.Config{CellKm: 5})}) // mock category: "Plombier"
		j := submit(t, st, domain.JobSpec{Query: tc.query, BBox: area, IncludeUnrelated: tc.all})
		j = waitDone(t, st, j.ID, 10*time.Second)
		stop()
		if got := j.Counters.Unique > 0; got != tc.want || j.Counters.RawFound == 0 {
			t.Errorf("query %q all=%v: unique=%d raw=%d", tc.query, tc.all, j.Counters.Unique, j.Counters.RawFound)
		}
	}
}

// countryGeocoder only knows Dubai worldwide, like Nominatim does when
// filtered on the wrong country.
type countryGeocoder struct{ calls []string }

func (g *countryGeocoder) CountryAt(context.Context, domain.Point) (string, error) { return "AE", nil }

func (g *countryGeocoder) Geocode(_ context.Context, q, country, _ string) (geocode.Result, error) {
	g.calls = append(g.calls, country)
	if country == "" || country == "AE" {
		return geocode.Result{BBox: domain.BBox{MinLat: 25.07, MinLng: 55.12, MaxLat: 25.09, MaxLng: 55.15}, Country: "AE"}, nil
	}
	return geocode.Result{}, geocode.ErrNotFound
}

func TestLocationAbroadAdoptsGeocodedCountry(t *testing.T) {
	st := memstore.New()
	gc := &countryGeocoder{}
	e := New(Config{PollMin: 5 * time.Millisecond, PollMax: 20 * time.Millisecond, FinalizeEvery: 20 * time.Millisecond},
		st, []source.Source{mock.New(mock.Config{CellKm: 5})}, gc, nil, metrics.New(), quietLog())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	j := submit(t, st, domain.JobSpec{Query: "plumber", Location: "Dubai"}) // no country
	j = waitDone(t, st, j.ID, 10*time.Second)
	if j.Status != domain.JobCompleted || j.Counters.Unique == 0 {
		t.Fatalf("job %+v", j)
	}
	if len(gc.calls) != 1 || gc.calls[0] != "" {
		t.Fatalf("geocoder calls %v, want one worldwide search", gc.calls)
	}
	// explicit wrong country: worldwide second try
	gc.calls = nil
	j2 := domain.Job{ID: "job_explicit", Spec: domain.JobSpec{Query: "plumber", Location: "Dubai", Country: "FR", Language: "fr"},
		Status: domain.JobQueued, CreatedAt: time.Now()}
	if err := st.CreateJob(context.Background(), &j2, []domain.Task{PlanTask(&j2)}); err != nil {
		t.Fatal(err)
	}
	waitDone(t, st, j2.ID, 10*time.Second)
	if len(gc.calls) != 2 || gc.calls[0] != "FR" || gc.calls[1] != "" {
		t.Fatalf("geocoder calls %v, want FR then worldwide", gc.calls)
	}
	rows, _ := st.ListJobBusinesses(context.Background(), j.ID, 0, 10, store.BusinessFilter{})
	if len(rows) == 0 || rows[0].Country != "AE" {
		t.Fatalf("records must carry the geocoded country, got %+v", rows)
	}
}
