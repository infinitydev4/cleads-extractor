package geo

import (
	"math"
	"testing"

	"github.com/cleads/business-extractor/internal/domain"
)

var lyon = domain.BBox{MinLat: 45.70, MinLng: 4.77, MaxLat: 45.81, MaxLng: 4.90}

func TestGridCoversBoxWithoutGaps(t *testing.T) {
	cells := Grid(lyon, 2, 0)
	h, w := SizeKm(lyon)
	wantRows, wantCols := int(math.Ceil(h/2)), int(math.Ceil(w/2))
	if len(cells) != wantRows*wantCols {
		t.Fatalf("got %d cells, want %d", len(cells), wantRows*wantCols)
	}
	var area float64
	for _, c := range cells {
		if !c.Valid() {
			t.Fatalf("invalid cell %+v", c)
		}
		area += (c.MaxLat - c.MinLat) * (c.MaxLng - c.MinLng)
	}
	total := (lyon.MaxLat - lyon.MinLat) * (lyon.MaxLng - lyon.MinLng)
	if math.Abs(area-total) > 1e-9 {
		t.Fatalf("cells cover %.10f, box is %.10f", area, total)
	}
}

func TestGridRespectsCap(t *testing.T) {
	france := domain.BBox{MinLat: 41.3, MinLng: -5.2, MaxLat: 51.1, MaxLng: 9.6}
	cells := Grid(france, 1, 500)
	if len(cells) > 500 || len(cells) < 300 {
		t.Fatalf("got %d cells, want <= 500 and close to the cap", len(cells))
	}
}

func TestSplit(t *testing.T) {
	q := Split(lyon)
	var area float64
	for _, c := range q {
		area += (c.MaxLat - c.MinLat) * (c.MaxLng - c.MinLng)
	}
	total := (lyon.MaxLat - lyon.MinLat) * (lyon.MaxLng - lyon.MinLng)
	if math.Abs(area-total) > 1e-12 {
		t.Fatal("split does not preserve area")
	}
}

func TestGeohash(t *testing.T) {
	// Reference value from the original geohash.org example.
	if got := Geohash(domain.Point{Lat: 57.64911, Lng: 10.40744}, 11); got != "u4pruydqqvj" {
		t.Fatalf("got %q", got)
	}
}

func TestHaversine(t *testing.T) {
	paris := domain.Point{Lat: 48.8566, Lng: 2.3522}
	lyonC := domain.Point{Lat: 45.7640, Lng: 4.8357}
	d := HaversineM(paris, lyonC) / 1000
	if d < 390 || d > 395 {
		t.Fatalf("Paris-Lyon = %.1f km", d)
	}
}

func TestBBoxAround(t *testing.T) {
	c := domain.Point{Lat: 45.76, Lng: 4.83}
	b := BBoxAround(c, 5000)
	h, w := SizeKm(b)
	if math.Abs(h-10) > 0.05 || math.Abs(w-10) > 0.2 {
		t.Fatalf("box %vx%v km", h, w)
	}
}

func TestZoomForCell(t *testing.T) {
	if z := ZoomForCell(1); z < 14 || z > 17 {
		t.Fatalf("zoom for 1km = %d", z)
	}
	if ZoomForCell(50) >= ZoomForCell(1) {
		t.Fatal("bigger cells must use lower zoom")
	}
}
