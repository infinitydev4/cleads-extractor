package osm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/source"
	"github.com/cleads/business-extractor/internal/taxonomy"
)

func TestBuildQuery(t *testing.T) {
	cell := domain.BBox{MinLat: 45.7, MinLng: 4.8, MaxLat: 45.8, MaxLng: 4.9}
	q := BuildQuery(source.Request{Sector: taxonomy.Get("plumber"), Cell: cell})
	if !strings.Contains(q, `nwr["craft"="plumber"](45.700000,4.800000,45.800000,4.900000);`) {
		t.Fatal(q)
	}
	q = BuildQuery(source.Request{Query: `pizza"]; out; [`, Cell: cell})
	if strings.Contains(q, `"];`) && strings.Count(q, "out") > 1 {
		t.Fatalf("query injection not neutralized:\n%s", q)
	}
}

func TestSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if !strings.Contains(r.Form.Get("data"), "craft") {
			t.Error("missing query")
		}
		_, _ = w.Write([]byte(`{"elements":[
			{"type":"node","id":1,"lat":45.76,"lon":4.84,"tags":{"name":"Dupont","craft":"plumber","phone":"+33 4 78 00 00 00;+33 6 00 00 00 00",
			 "addr:housenumber":"1","addr:street":"Rue X","addr:postcode":"69003","addr:city":"Lyon","website":"dupont.fr",
			 "email":"contact@dupont.fr","ref:FR:SIRET":"732 829 320 00074"}},
			{"type":"way","id":2,"center":{"lat":45.77,"lon":4.85},"tags":{"name":"Martin","craft":"plumber"}},
			{"type":"node","id":3,"lat":45.7,"lon":4.8,"tags":{"craft":"plumber"}}]}`))
	}))
	defer srv.Close()
	s := New(Config{Endpoints: []string{srv.URL}})
	res, err := s.Search(context.Background(), source.Request{Sector: taxonomy.Get("plumber"),
		Cell: domain.BBox{MinLat: 45.7, MinLng: 4.8, MaxLat: 45.8, MaxLng: 4.9}, Country: "FR"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Businesses) != 2 {
		t.Fatalf("unnamed elements must be skipped, got %d", len(res.Businesses))
	}
	b := res.Businesses[0]
	if b.OSMID != "node/1" || b.Phone != "+33 4 78 00 00 00" || b.SIRET != "73282932000074" ||
		b.PostalCode != "69003" || len(b.Emails) != 1 || b.Address != "1 Rue X, 69003 Lyon" {
		t.Fatalf("bad mapping %+v", b)
	}
	if res.Businesses[1].Lat != 45.77 {
		t.Fatal("way center not used")
	}
}

func TestFailoverToMirrorWhenOverloaded(t *testing.T) {
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer busy.Close()
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"elements":[{"type":"node","id":1,"lat":45.76,"lon":4.84,"tags":{"name":"A","craft":"plumber"}}]}`))
	}))
	defer ok.Close()
	s := New(Config{Endpoints: []string{busy.URL, ok.URL}})
	cell := domain.BBox{MinLat: 45.7, MinLng: 4.8, MaxLat: 45.8, MaxLng: 4.9}
	for i := 0; i < 2; i++ { // whichever endpoint the rotation starts with
		res, err := s.Search(context.Background(), source.Request{Sector: taxonomy.Get("plumber"), Cell: cell})
		if err != nil || len(res.Businesses) != 1 {
			t.Fatalf("failover failed: %v %+v", err, res)
		}
	}
	allBusy := New(Config{Endpoints: []string{busy.URL}})
	_, err := allBusy.Search(context.Background(), source.Request{Sector: taxonomy.Get("plumber"), Cell: cell})
	if !errors.Is(err, source.ErrRateLimited) {
		t.Fatalf("overloaded instance must map to ErrRateLimited, got %v", err)
	}
}
