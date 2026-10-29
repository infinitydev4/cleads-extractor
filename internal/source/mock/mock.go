// Package mock is a deterministic synthetic source used by tests and by the
// end-to-end benchmark. It emulates the statistical shape of a real source:
// network latency, overlap between neighbouring cells (duplicates), partial
// contact data and paginated results.
package mock

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/source"
)

const Name = "mock"

type Config struct {
	PerPage     int           // results per page
	Pages       int           // pages per cell
	Latency     time.Duration // simulated network latency (mean)
	DensityKm2  float64       // businesses per km²; drives overlap
	PhoneRatio  float64       // share of records with a phone
	SiteRatio   float64       // share with a website
	FailRatio   float64       // transient error probability
	Concurrency int
	RatePerSec  float64
	CellKm      float64
}

type Source struct{ cfg Config }

func New(cfg Config) *Source {
	if cfg.PerPage == 0 {
		cfg.PerPage = 20
	}
	if cfg.Pages == 0 {
		cfg.Pages = 3
	}
	if cfg.DensityKm2 == 0 {
		cfg.DensityKm2 = 40
	}
	if cfg.PhoneRatio == 0 {
		cfg.PhoneRatio = 0.85
	}
	if cfg.SiteRatio == 0 {
		cfg.SiteRatio = 0.55
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 64
	}
	if cfg.RatePerSec == 0 {
		cfg.RatePerSec = 10_000
	}
	if cfg.CellKm == 0 {
		cfg.CellKm = 1
	}
	return &Source{cfg: cfg}
}

func (s *Source) Info() source.Info {
	return source.Info{
		Name: Name, Description: "Synthetic source for tests and benchmarks.",
		DefaultCellKm: s.cfg.CellKm, MaxCells: 100_000, MinCellKm: 0.1,
		RatePerSec: s.cfg.RatePerSec, Burst: int(s.cfg.RatePerSec), Concurrency: s.cfg.Concurrency,
	}
}

func (s *Source) Search(ctx context.Context, req source.Request) (source.Result, error) {
	seed := hash(fmt.Sprintf("%s|%.5f|%.5f|%d", req.JobID, req.Cell.MinLat, req.Cell.MinLng, req.Page))
	rng := rand.New(rand.NewPCG(seed, seed>>1))
	if s.cfg.Latency > 0 {
		jitter := time.Duration(rng.NormFloat64() * float64(s.cfg.Latency) / 4)
		select {
		case <-ctx.Done():
			return source.Result{}, ctx.Err()
		case <-time.After(max(time.Millisecond, s.cfg.Latency+jitter)):
		}
	}
	// Failures are drawn independently per attempt (global RNG), while the
	// data itself stays deterministic per cell/page.
	if s.cfg.FailRatio > 0 && rand.Float64() < s.cfg.FailRatio {
		return source.Result{}, fmt.Errorf("mock transient failure")
	}
	out := source.Result{Businesses: make([]domain.Business, 0, s.cfg.PerPage)}
	// Businesses live on a fixed lattice so that overlapping or adjacent
	// queries return the same entities, exactly like a real index.
	step := 1 / math.Sqrt(s.cfg.DensityKm2) / 111.32 // degrees between businesses
	// Each query "sees" a 20% wider area than its cell (search engines leak).
	padLat := (req.Cell.MaxLat - req.Cell.MinLat) * 0.1
	padLng := (req.Cell.MaxLng - req.Cell.MinLng) * 0.1
	i0 := int(math.Floor((req.Cell.MinLat - padLat) / step))
	j0 := int(math.Floor((req.Cell.MinLng - padLng) / step))
	i1 := int(math.Ceil((req.Cell.MaxLat + padLat) / step))
	j1 := int(math.Ceil((req.Cell.MaxLng + padLng) / step))
	total := (i1 - i0) * (j1 - j0)
	start := req.Page * s.cfg.PerPage
	n := 0
	for k := start; k < total && n < s.cfg.PerPage; k++ {
		i := i0 + k/(j1-j0)
		j := j0 + k%(j1-j0)
		out.Businesses = append(out.Businesses, s.business(i, j, step, req.Country))
		n++
	}
	if start+n < total {
		if req.Page+1 < s.cfg.Pages {
			out.NextPageToken = strconv.Itoa(req.Page + 1)
		} else {
			out.Saturated = true
		}
	}
	return out, nil
}

func (s *Source) business(i, j int, step float64, country string) domain.Business {
	id := uint64(int64(i))*1_000_003 ^ uint64(int64(j))
	r := rand.New(rand.NewPCG(id, id^0x9e3779b97f4a7c15))
	b := domain.Business{
		Name:         fmt.Sprintf("Entreprise %d-%d", i, j),
		Category:     "Plombier",
		Categories:   []string{"Plombier"},
		Lat:          float64(i) * step,
		Lng:          float64(j) * step,
		Street:       fmt.Sprintf("%d rue de la Paix", 1+r.IntN(200)),
		PostalCode:   fmt.Sprintf("%05d", 1000+r.IntN(94000)),
		City:         "Ville",
		Country:      country,
		PlaceID:      "mock_" + strconv.FormatUint(id, 36),
		Status:       domain.StatusOperational,
		Sources:      []string{Name},
		Rating:       math.Round((3+2*r.Float64())*10) / 10,
		ReviewsCount: r.IntN(400),
	}
	b.Address = b.Street + ", " + b.PostalCode + " " + b.City
	if r.Float64() < s.cfg.PhoneRatio {
		b.Phone = fmt.Sprintf("0%d%08d", 1+r.IntN(5), r.IntN(100_000_000))
	}
	if r.Float64() < s.cfg.SiteRatio {
		b.Website = fmt.Sprintf("https://www.entreprise-%d-%d.fr/", i, j)
	}
	return b
}

func hash(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}
