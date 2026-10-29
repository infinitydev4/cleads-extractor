package gmapshttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/source"
)

func TestParsePlaceFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/place_raw.json")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParsePlace(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := domain.Business{
		Name: "Kipriakon", Category: "Restaurant", City: "Limassol", PostalCode: "3042",
		PlaceID: "ChIJDdnwdv0y5xQRRytw1ihZQeU", CID: "16519582940102929223", Phone: "25101555",
		ReviewsCount: 396, Rating: 4.2, Status: domain.StatusOperational,
	}
	if b.Name != want.Name || b.Category != want.Category || b.City != want.City || b.PostalCode != want.PostalCode ||
		b.PlaceID != want.PlaceID || b.CID != want.CID || b.Phone != want.Phone || b.ReviewsCount != want.ReviewsCount ||
		b.Rating != want.Rating || b.Status != want.Status {
		t.Fatalf("unexpected parse:\n got %+v\nwant %+v", b, want)
	}
	if b.Lat < 34.6 || b.Lat > 34.7 || b.Lng < 33 || b.Lng > 33.1 {
		t.Fatalf("bad coordinates %v,%v", b.Lat, b.Lng)
	}
	if len(b.Hours) != 7 {
		t.Fatalf("want 7 days of hours, got %v", b.Hours)
	}
}

func TestParsePlaceRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "<html>", "[1,2,3]", `{"a":1}`} {
		if _, err := ParsePlace([]byte(in)); err == nil {
			t.Errorf("ParsePlace(%q) accepted garbage", in)
		}
	}
}

// searchPayload builds a /search?tbm=map response with n businesses using
// the same positional layout as Google.
func searchPayload(n int) []byte {
	items := []any{[]any{"metadata"}}
	for i := 0; i < n; i++ {
		biz := make([]any, 204)
		biz[11] = "Plomberie " + string(rune('A'+i%26))
		biz[13] = []any{"Plombier", "Chauffagiste"}
		biz[7] = []any{"/url?q=https://plomberie-" + string(rune('a'+i%26)) + ".fr/&opi=1"}
		biz[4] = []any{nil, nil, "€€", nil, nil, nil, nil, 4.5, float64(10 + i)}
		biz[9] = []any{nil, nil, 45.76 + float64(i)/1000, 4.83}
		biz[10] = "0x47f4ea516ae88797:0x408ab2ae4bb21f0"
		biz[39] = "1 Rue de la République, 69002 Lyon"
		biz[78] = "ChIJtest" + string(rune('A'+i%26))
		biz[178] = []any{[]any{"04 78 00 00 0" + string(rune('0'+i%10))}}
		biz[183] = []any{nil, []any{nil, "1 Rue de la République", nil, "Lyon", "69002", "Auvergne-Rhône-Alpes", "FR"}}
		item := make([]any, 15)
		item[14] = biz
		items = append(items, item)
	}
	body, _ := json.Marshal([]any{[]any{"q", items}})
	return append([]byte(")]}'\n"), body...)
}

func TestParseSearch(t *testing.T) {
	got, err := ParseSearch(searchPayload(3))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 results, got %d", len(got))
	}
	b := got[0]
	if b.Name != "Plomberie A" || b.Website != "https://plomberie-a.fr/" || b.PostalCode != "69002" ||
		b.City != "Lyon" || b.Rating != 4.5 || b.ReviewsCount != 10 || b.CID != "290670376679842288" ||
		b.Phone != "0478000000" || b.Address != "1 Rue de la République, 69002 Lyon" {
		t.Fatalf("unexpected: %+v", b)
	}
}

func TestSearchHTTP(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		if !strings.Contains(r.Header.Get("Cookie"), "SOCS=") {
			t.Error("consent cookie missing")
		}
		_, _ = w.Write(searchPayload(20))
	}))
	defer srv.Close()
	s := New(Config{Endpoint: srv.URL, MaxPages: 2})
	req := source.Request{Query: "plombier", Language: "fr", Country: "FR",
		Cell: domain.BBox{MinLat: 45.7, MinLng: 4.8, MaxLat: 45.8, MaxLng: 4.9}}
	res, err := s.Search(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Businesses) != 20 || res.NextPageToken == "" || res.Saturated {
		t.Fatalf("page 0: %d results, token %q, saturated %v", len(res.Businesses), res.NextPageToken, res.Saturated)
	}
	if !strings.Contains(gotQuery, "tbm=map") || !strings.Contains(gotQuery, "q=plombier") {
		t.Fatalf("query = %s", gotQuery)
	}
	req.Page = 1
	res, err = s.Search(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.NextPageToken != "" || !res.Saturated {
		t.Fatal("last page full must report saturation")
	}
}

func TestSearchDetectsBlocking(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/sorry/") {
			_, _ = w.Write([]byte("<html>unusual traffic</html>"))
			return
		}
		http.Redirect(w, r, "/sorry/index?continue=x", http.StatusFound)
	}))
	defer srv.Close()
	s := New(Config{Endpoint: srv.URL})
	_, err := s.Search(context.Background(), source.Request{Query: "x", Cell: domain.BBox{MinLat: 1, MinLng: 1, MaxLat: 2, MaxLng: 2}})
	if !errors.Is(err, source.ErrBlocked) {
		t.Fatalf("want ErrBlocked, got %v", err)
	}
}

func TestCIDFromDataID(t *testing.T) {
	if got := cidFromDataID("0x14e732fd76f0d90d:0xe5415928d6702b47"); got != "16519582940102929223" {
		t.Fatal(got)
	}
	if cidFromDataID("garbage") != "" {
		t.Fatal("garbage accepted")
	}
}

// The two benchmarks compare the lazy gjson parser with the approach of
// decoding the whole payload into []any (what most implementations do).
func BenchmarkParsePlaceGJSON(b *testing.B) {
	raw, _ := os.ReadFile("testdata/place_raw.json")
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ParsePlace(raw); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParsePlaceUnmarshalAny(b *testing.B) {
	raw, _ := os.ReadFile("testdata/place_raw.json")
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	for b.Loop() {
		var jd []any
		if err := json.Unmarshal(raw, &jd); err != nil {
			b.Fatal(err)
		}
		d, _ := jd[6].([]any)
		_, _ = d[11].(string)
	}
}

func BenchmarkParseSearch20(b *testing.B) {
	payload := searchPayload(20)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ParseSearch(payload); err != nil {
			b.Fatal(err)
		}
	}
}
