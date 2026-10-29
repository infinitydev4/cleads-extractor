package sirene

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/source"
	"github.com/cleads/business-extractor/internal/taxonomy"
)

const sample = `{"results":[{"siren":"732829320","nom_complet":"DUPONT PLOMBERIE","activite_principale":"43.22A",
"etat_administratif":"A","matching_etablissements":[{"siret":"73282932000074","adresse":"1 RUE X 69003 LYON",
"code_postal":"69003","libelle_commune":"LYON","latitude":"45.76","longitude":"4.84","activite_principale":"43.22A",
"etat_administratif":"A","liste_enseignes":["PLOMBERIE DU RHONE"]},
{"siret":"73282932000082","activite_principale":"45.3E","etat_administratif":"F","latitude":"45.76","longitude":"4.84"},
{"siret":"73282932000090","activite_principale":"43.91B","etat_administratif":"A","latitude":"45.76","longitude":"4.84"},
{"siret":"73282932000108","activite_principale":"43.22A","etat_administratif":"A","statut_diffusion_etablissement":"P","latitude":"45.76","longitude":"4.84"}]},
{"siren":"111111118","statut_diffusion":"P","activite_principale":"43.22A","matching_etablissements":[{"siret":"11111111800010","activite_principale":"43.22A","latitude":"45.76","longitude":"4.84"}]}],"page":1,"total_pages":3}`

func TestNearPoint(t *testing.T) {
	var q string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/near_point" {
			t.Errorf("path %s", r.URL.Path)
		}
		q = r.URL.RawQuery
		_, _ = w.Write([]byte(sample))
	}))
	defer srv.Close()
	s := New(Config{Endpoint: srv.URL, MaxPages: 2})
	cell := domain.BBox{MinLat: 45.7, MinLng: 4.8, MaxLat: 45.8, MaxLng: 4.9}
	res, err := s.Search(context.Background(), source.Request{Sector: taxonomy.Get("plumber"), Cell: cell})
	if err != nil {
		t.Fatal(err)
	}
	// closed, other-activity and non-diffusible (statut P) records are dropped
	if len(res.Businesses) != 1 || res.NextPageToken != "2" {
		t.Fatalf("got %+v", res)
	}
	b := res.Businesses[0]
	if b.SIRET != "73282932000074" || b.SIREN != "732829320" || b.NAF != "43.22A" ||
		b.Name != "Plomberie du Rhone" || b.City != "Lyon" || b.Lat != 45.76 {
		t.Fatalf("bad mapping %+v", b)
	}
	if q == "" || !contains(q, "activite_principale=43.22A") {
		t.Fatalf("query %s", q)
	}
	res, _ = s.Search(context.Background(), source.Request{Sector: taxonomy.Get("plumber"), Cell: cell, Page: 1})
	if !res.Saturated || res.NextPageToken != "" {
		t.Fatal("page cap must report saturation")
	}
}

func TestRequiresSector(t *testing.T) {
	s := New(Config{})
	_, err := s.Search(context.Background(), source.Request{Query: "plombier"})
	if !errors.Is(err, source.ErrPermanent) {
		t.Fatal(err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
