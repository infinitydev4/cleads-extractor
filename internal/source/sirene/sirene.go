// Package sirene queries the French public company registry through the
// "API Recherche d'entreprises" (recherche-entreprises.api.gouv.fr, Licence
// Ouverte Etalab). It needs no key and returns legal identifiers (SIREN,
// SIRET, NAF) that Google does not have, which makes it the backbone of an
// independent, reusable prospecting base for France.
package sirene

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/geo"
	"github.com/cleads/business-extractor/internal/source"
)

const (
	Name            = "sirene"
	defaultEndpoint = "https://recherche-entreprises.api.gouv.fr"
	perPage         = 25
	maxRadiusKm     = 50
)

type Config struct {
	Endpoint    string
	HTTPClient  *http.Client
	MaxPages    int
	RatePerSec  float64
	Concurrency int
}

type Source struct{ cfg Config }

func New(cfg Config) *Source {
	if cfg.Endpoint == "" {
		cfg.Endpoint = defaultEndpoint
	}
	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = source.NewHTTPClient(20*time.Second, 8, nil)
	}
	if cfg.MaxPages == 0 {
		cfg.MaxPages = 40
	}
	if cfg.RatePerSec == 0 {
		cfg.RatePerSec = 6 // documented limit is 7 req/s per IP
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 4
	}
	return &Source{cfg: cfg}
}

func (s *Source) Info() source.Info {
	return source.Info{
		Name:          Name,
		Description:   "French company registry (SIRENE) via recherche-entreprises.api.gouv.fr — open licence, SIREN/SIRET/NAF, no phone/website.",
		Official:      true,
		DefaultCellKm: 10,
		MaxCells:      150,
		MinCellKm:     2,
		RatePerSec:    s.cfg.RatePerSec,
		Burst:         2,
		Concurrency:   s.cfg.Concurrency,
	}
}

// flexFloat accepts numbers and numeric strings.
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	*f = flexFloat(v)
	return nil
}

type etablissement struct {
	Siret              string    `json:"siret"`
	Adresse            string    `json:"adresse"`
	CodePostal         string    `json:"code_postal"`
	LibelleCommune     string    `json:"libelle_commune"`
	Latitude           flexFloat `json:"latitude"`
	Longitude          flexFloat `json:"longitude"`
	ActivitePrincipale string    `json:"activite_principale"`
	EtatAdministratif  string    `json:"etat_administratif"`
	ListeEnseignes     []string  `json:"liste_enseignes"`
	NomCommercial      string    `json:"nom_commercial"`
	StatutDiffusion    string    `json:"statut_diffusion_etablissement"`
}

type result struct {
	Siren                  string          `json:"siren"`
	NomComplet             string          `json:"nom_complet"`
	NomRaisonSociale       string          `json:"nom_raison_sociale"`
	ActivitePrincipale     string          `json:"activite_principale"`
	EtatAdministratif      string          `json:"etat_administratif"`
	StatutDiffusion        string          `json:"statut_diffusion"`
	Siege                  etablissement   `json:"siege"`
	MatchingEtablissements []etablissement `json:"matching_etablissements"`
}

type response struct {
	Results    []result `json:"results"`
	Page       int      `json:"page"`
	TotalPages int      `json:"total_pages"`
}

func (s *Source) Search(ctx context.Context, req source.Request) (source.Result, error) {
	if req.Sector == nil || len(req.Sector.NAF) == 0 {
		return source.Result{}, fmt.Errorf("%w: sirene requires a sector with NAF codes", source.ErrPermanent)
	}
	if !req.Cell.Valid() {
		return source.Result{}, fmt.Errorf("%w: sirene requires a geographic cell", source.ErrPermanent)
	}
	c := req.Cell.Center()
	h, w := geo.SizeKm(req.Cell)
	radius := math.Min(maxRadiusKm, math.Max(0.5, math.Hypot(h, w)/2))
	page := req.Page + 1

	q := url.Values{}
	q.Set("lat", strconv.FormatFloat(c.Lat, 'f', 6, 64))
	q.Set("long", strconv.FormatFloat(c.Lng, 'f', 6, 64))
	q.Set("radius", strconv.FormatFloat(radius, 'f', 2, 64))
	q.Set("activite_principale", strings.Join(req.Sector.NAF, ","))
	q.Set("etat_administratif", "A")
	q.Set("page", strconv.Itoa(page))
	q.Set("per_page", strconv.Itoa(perPage))

	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.Endpoint+"/near_point?"+q.Encode(), nil)
	if err != nil {
		return source.Result{}, err
	}
	hreq.Header.Set("Accept", "application/json")
	resp, err := s.cfg.HTTPClient.Do(hreq)
	if err != nil {
		return source.Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return source.Result{}, source.ClassifyHTTP(resp, raw)
	}
	var r response
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&r); err != nil {
		return source.Result{}, fmt.Errorf("decode sirene: %w", err)
	}
	// The activity filter applies to the company (unité légale), but
	// matching_etablissements lists all its nearby establishments, including
	// closed ones and ones with another activity: filter them here.
	wanted := make(map[string]struct{}, len(req.Sector.NAF))
	for _, n := range req.Sector.NAF {
		wanted[n] = struct{}{}
	}
	out := source.Result{Businesses: make([]domain.Business, 0, len(r.Results))}
	for i := range r.Results {
		res := &r.Results[i]
		// "P" = diffusion partielle: the person objected to the reuse of
		// their data (art. A123-96 Code de commerce). Never prospect them.
		if res.StatutDiffusion == "P" {
			continue
		}
		etabs := res.MatchingEtablissements
		if len(etabs) == 0 {
			etabs = []etablissement{res.Siege}
		}
		for j := range etabs {
			e := &etabs[j]
			if e.EtatAdministratif == "F" || e.StatutDiffusion == "P" {
				continue
			}
			if _, ok := wanted[e.ActivitePrincipale]; e.ActivitePrincipale != "" && !ok {
				continue
			}
			p := domain.Point{Lat: float64(e.Latitude), Lng: float64(e.Longitude)}
			if p.Lat != 0 && !req.Cell.Contains(p) && geo.HaversineM(c, p) > radius*1000 {
				continue
			}
			out.Businesses = append(out.Businesses, toBusiness(res, e))
		}
	}
	if page < r.TotalPages {
		if page < s.cfg.MaxPages {
			out.NextPageToken = strconv.Itoa(page + 1)
		} else {
			out.Saturated = true
		}
	}
	return out, nil
}

func toBusiness(r *result, e *etablissement) domain.Business {
	name := e.NomCommercial
	if name == "" && len(e.ListeEnseignes) > 0 {
		name = e.ListeEnseignes[0]
	}
	if name == "" {
		name = r.NomComplet
	}
	naf := e.ActivitePrincipale
	if naf == "" {
		naf = r.ActivitePrincipale
	}
	b := domain.Business{
		Name:       titleCase(name),
		Address:    e.Adresse,
		PostalCode: e.CodePostal,
		City:       titleCase(e.LibelleCommune),
		Country:    "FR",
		Lat:        float64(e.Latitude),
		Lng:        float64(e.Longitude),
		SIREN:      r.Siren,
		SIRET:      e.Siret,
		NAF:        naf,
		Category:   naf,
		Sources:    []string{Name},
		SourceURL:  "https://annuaire-entreprises.data.gouv.fr/etablissement/" + e.Siret,
		Status:     domain.StatusOperational,
	}
	if e.EtatAdministratif == "F" || r.EtatAdministratif == "C" {
		b.Status = domain.StatusClosedPermanently
	}
	return b
}

// titleCase turns registry upper-case names into display case.
func titleCase(s string) string {
	if s == "" || strings.ToUpper(s) != s {
		return s
	}
	words := strings.Fields(strings.ToLower(s))
	for i, w := range words {
		r := []rune(w)
		if len(r) > 0 && (len(r) > 2 || i == 0) {
			r[0] = []rune(strings.ToUpper(string(r[0])))[0]
		}
		words[i] = string(r)
	}
	return strings.Join(words, " ")
}
