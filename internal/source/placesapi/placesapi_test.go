package placesapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/source"
	"github.com/cleads/business-extractor/internal/taxonomy"
)

func fakePlaces(t *testing.T, perPage int, token string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Goog-Api-Key") != "k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !strings.Contains(r.Header.Get("X-Goog-FieldMask"), "places.websiteUri") {
			t.Error("contact tier must request websiteUri")
		}
		var body searchBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.LocationRestriction == nil {
			t.Error("cell must become a location restriction")
		}
		places := make([]map[string]any, perPage)
		for i := range places {
			places[i] = map[string]any{
				"id":                       fmt.Sprintf("ChIJ%d", i),
				"displayName":              map[string]any{"text": fmt.Sprintf("Plombier %d", i)},
				"formattedAddress":         "1 Rue X, 69003 Lyon, France",
				"location":                 map[string]any{"latitude": 45.76, "longitude": 4.84},
				"internationalPhoneNumber": "+33 4 78 00 00 00",
				"websiteUri":               "https://plombier.fr/",
				"businessStatus":           "OPERATIONAL",
				"googleMapsUri":            "https://maps.google.com/?cid=123456789",
				"userRatingCount":          12,
				"rating":                   4.8,
				"primaryTypeDisplayName":   map[string]any{"text": "Plombier"},
				"addressComponents": []map[string]any{
					{"longText": "69003", "types": []string{"postal_code"}},
					{"longText": "Lyon", "types": []string{"locality", "political"}},
					{"longText": "France", "shortText": "FR", "types": []string{"country"}},
				},
				"regularOpeningHours": map[string]any{"weekdayDescriptions": []string{"lundi: 08:00–18:00"}},
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"places": places, "nextPageToken": token})
	}))
}

func TestSearch(t *testing.T) {
	srv := fakePlaces(t, 20, "tok")
	defer srv.Close()
	s, err := New(Config{APIKey: "k", Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	req := source.Request{Query: "plombier", Language: "fr", Country: "FR",
		Cell: domain.BBox{MinLat: 45.7, MinLng: 4.8, MaxLat: 45.8, MaxLng: 4.9}}
	res, err := s.Search(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Businesses) != 20 || res.NextPageToken != "tok" {
		t.Fatalf("got %d, token %q", len(res.Businesses), res.NextPageToken)
	}
	b := res.Businesses[0]
	if b.CID != "123456789" || b.City != "Lyon" || b.PostalCode != "69003" || b.Country != "FR" ||
		b.Hours["lundi"] != "08:00–18:00" || b.Phone == "" || b.Category != "Plombier" {
		t.Fatalf("bad mapping: %+v", b)
	}
	req.Page = 2
	res, _ = s.Search(context.Background(), req)
	if res.NextPageToken != "" || !res.Saturated {
		t.Fatal("third full page must be saturated with no further token")
	}
}

func TestStrictTypeWhenSectorOnly(t *testing.T) {
	var body searchBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	s, _ := New(Config{APIKey: "k", Endpoint: srv.URL})
	_, err := s.Search(context.Background(), source.Request{Sector: taxonomy.Get("plumber"),
		Cell: domain.BBox{MinLat: 1, MinLng: 1, MaxLat: 2, MaxLng: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if body.IncludedType != "plumber" || !body.StrictTypeFiltering || body.TextQuery != "plombier" {
		t.Fatalf("body = %+v", body)
	}
}

func TestErrorsClassified(t *testing.T) {
	for code, want := range map[int]error{429: source.ErrRateLimited, 403: source.ErrBlocked, 400: source.ErrPermanent} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(code)
		}))
		s, _ := New(Config{APIKey: "k", Endpoint: srv.URL})
		_, err := s.Search(context.Background(), source.Request{Query: "x"})
		srv.Close()
		if !errors.Is(err, want) {
			t.Errorf("status %d: got %v, want %v", code, err, want)
		}
		if code == 429 && source.RetryAfter(err).Seconds() != 7 {
			t.Errorf("Retry-After not propagated: %v", source.RetryAfter(err))
		}
	}
}
