package gmapsbrowser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/source"
)

const csvExport = `input_id,link,title,category,address,open_hours,popular_times,website,phone,plus_code,review_count,review_rating,reviews_per_rating,latitude,longitude,cid,status,descriptions,reviews_link,thumbnail,timezone,price_range,data_id,street_view_url,place_id,images,reservations,order_online,menu,owner,complete_address,credit_cards_accepted,about,user_reviews,user_reviews_extended,emails
x,https://maps/x,Dupont Plomberie,Plombier,"1 Rue X, 69003 Lyon","{""Monday"":[""8 AM–6 PM""]}",,https://dupont.fr/,04 78 00 00 00,,12,4.8,,45.76,4.84,123,Open,,,,,,,,ChIJ1,,,,,,"{""street"":""1 Rue X"",""city"":""Lyon"",""postal_code"":""69003"",""state"":""ARA""}",,,,,"contact@dupont.fr, devis@dupont.fr"
`

func TestParseCSV(t *testing.T) {
	items, err := ParseCSV(strings.NewReader(csvExport), "FR")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d", len(items))
	}
	b := items[0]
	if b.Name != "Dupont Plomberie" || b.PostalCode != "69003" || b.City != "Lyon" || len(b.Emails) != 2 ||
		b.Hours["Monday"] != "8 AM–6 PM" || b.ReviewsCount != 12 || b.PlaceID != "ChIJ1" || b.Status != domain.StatusOperational {
		t.Fatalf("bad mapping %+v", b)
	}
}

func TestSidecarFlow(t *testing.T) {
	var polls, deleted atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/jobs":
			var j upstreamJob
			_ = json.NewDecoder(r.Body).Decode(&j)
			if len(j.Keywords) != 1 || j.Lat == "" || j.Zoom == 0 {
				t.Errorf("bad upstream job %+v", j)
			}
			_, _ = w.Write([]byte(`{"id":"u1"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/jobs/u1":
			st := "working"
			if polls.Add(1) >= 2 {
				st = "ok"
			}
			_, _ = w.Write([]byte(`{"ID":"u1","Status":"` + st + `"}`))
		case r.URL.Path == "/api/v1/jobs/u1/download":
			_, _ = w.Write([]byte(csvExport))
		case r.Method == http.MethodDelete:
			deleted.Add(1)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	s, _ := New(Config{BaseURL: srv.URL, PollEvery: 10 * time.Millisecond})
	res, err := s.Search(context.Background(), source.Request{Query: "plombier", Language: "fr",
		Cell: domain.BBox{MinLat: 45.7, MinLng: 4.8, MaxLat: 45.8, MaxLng: 4.9}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Businesses) != 1 || deleted.Load() != 1 {
		t.Fatalf("results %d, deleted %d", len(res.Businesses), deleted.Load())
	}
}
