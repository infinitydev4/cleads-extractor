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
		PlaceID: "ChIJDdnwdv0y5xQRRytw1ihZQeU", CID: "16519582940102929223", Phone: "+35725101555",
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
		Cell: domain.BBox{MinLat: 45.75, MinLng: 4.82, MaxLat: 45.79, MaxLng: 4.84}}
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

func TestParseLiveSearchFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/search_live.json")
	if err != nil {
		t.Fatal(err)
	}
	items, err := ParseSearch(raw)
	if err != nil || len(items) != 20 {
		t.Fatalf("got %d items, err %v", len(items), err)
	}
	withPhone, withSite := 0, 0
	for _, b := range items {
		if b.Phone != "" {
			withPhone++
		}
		if b.Website != "" {
			withSite++
		}
		if b.DataID == "" || b.PlaceID == "" || b.CID == "" || b.Status != domain.StatusOperational {
			t.Fatalf("identifiers/status missing: %+v", b)
		}
	}
	if withPhone < 15 || withSite < 10 {
		t.Fatalf("phones %d sites %d", withPhone, withSite)
	}
}

func TestDetailsFromLiveFixture(t *testing.T) {
	payload, err := os.ReadFile("testdata/preview_place_live.json")
	if err != nil {
		t.Fatal(err)
	}
	var gotPB string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPB = r.URL.Query().Get("pb")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	s := New(Config{DetailsEndpoint: srv.URL})
	b, err := s.Details(context.Background(), source.DetailsRequest{DataID: "0x47f4ebd905024249:0xcf3cd1724f630f1b", Name: "CHAPUIS PLOMBERIE", Language: "fr", Country: "FR"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(gotPB, "!1m15!1s0x47f4ebd905024249:0xcf3cd1724f630f1b!2sCHAPUIS PLOMBERIE!3m12") || !strings.Contains(gotPB, "!14m3!1s") {
		t.Fatalf("pb = %.80s", gotPB)
	}
	d := b.Details
	if d == nil || b.DetailsAt.IsZero() || len(b.Categories) < 5 || d.Owner == nil || len(d.Reviews) == 0 ||
		d.ReviewsPerRating[5] != 70 || d.PlusCode == "" || d.Timezone != "Europe/Paris" || len(d.Images) == 0 {
		t.Fatalf("details incomplete: %+v / %+v", b, d)
	}
	r := d.Reviews[0]
	if r.Author == "" || r.Rating != 5 || r.Text == "" || r.PublishedAt.IsZero() {
		t.Fatalf("review %+v", r)
	}
}

func TestDetailsFixtureAttributes(t *testing.T) {
	raw, _ := os.ReadFile("testdata/place_raw.json")
	b, err := ParsePlace(raw)
	if err != nil {
		t.Fatal(err)
	}
	d := b.Details
	if len(d.Attributes) != 10 || len(d.Images) == 0 || len(d.PopularTimes) != 7 {
		t.Fatalf("attrs %d images %d popular %d", len(d.Attributes), len(d.Images), len(d.PopularTimes))
	}
}

func TestPaginationStopsWhenResultsLeaveTheCell(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(searchPayload(20)) // all results around 45.76,4.83
	}))
	defer srv.Close()
	s := New(Config{Endpoint: srv.URL})
	far := domain.BBox{MinLat: 48.80, MinLng: 2.30, MaxLat: 48.82, MaxLng: 2.33}
	res, err := s.Search(context.Background(), source.Request{Query: "x", Cell: far})
	if err != nil {
		t.Fatal(err)
	}
	if res.NextPageToken != "" || res.Saturated {
		t.Fatal("results outside the cell must stop pagination")
	}
}

func TestStatusAndDescriptionFromSlot88(t *testing.T) {
	if closedMarker("Cathédrale avec trésors du XIVe siècle") != "" || closedMarker("FERMÉ") == "" || closedMarker("CLOSED") == "" {
		t.Fatal("closedMarker")
	}
	cases := map[[2]string]string{
		{"", ""}: "", {"", "FERMÉ"}: domain.StatusClosedPermanently, {"", "FERMÉ TEMPORAIREMENT"}: domain.StatusClosedTemporarily,
		{"Temporarily closed", ""}: domain.StatusClosedTemporarily, {"Open now", ""}: domain.StatusOperational,
	}
	for in, want := range cases {
		if got := mapStatus(in[0], in[1]); got != want {
			t.Errorf("mapStatus(%q,%q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

func TestParseLiveDubaiFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/search_live_dubai.json")
	if err != nil {
		t.Fatal(err)
	}
	items, err := ParseSearch(raw)
	if err != nil || len(items) < 10 {
		t.Fatalf("items %d err %v", len(items), err)
	}
	b := items[0]
	if b.Phone != "+971566623611" || b.Country != "AE" || b.City != "Dubai" || b.Street != "Botanica Tower" {
		t.Fatalf("international fields: phone=%q country=%q city=%q street=%q", b.Phone, b.Country, b.City, b.Street)
	}
	for _, it := range items {
		if it.Phone != "" && !strings.HasPrefix(it.Phone, "+971") {
			t.Errorf("%s: phone %q not international", it.Name, it.Phone)
		}
	}
}

func TestFrenchPhonesStayInternational(t *testing.T) {
	raw, _ := os.ReadFile("testdata/search_live.json")
	items, _ := ParseSearch(raw)
	for _, it := range items {
		if it.Phone != "" && !strings.HasPrefix(it.Phone, "+33") {
			t.Fatalf("%s: phone %q", it.Name, it.Phone)
		}
		// service-area businesses have no address block: the source fills
		// the country from the request instead
		if it.Address != "" && it.Country != "FR" {
			t.Fatalf("%s: country %q", it.Name, it.Country)
		}
	}
}
