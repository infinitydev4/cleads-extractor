// Package geo provides the geographic primitives of the planner: grid tiling,
// distances and geohashes. Search engines cap results per query (Google: ~120
// for Maps, 60 for Places Text Search), so large areas must be split into
// cells to reach exhaustiveness. The tiling idea comes from
// gosom/google-maps-scraper (MIT); this is an independent implementation.
package geo

import (
	"math"

	"github.com/cleads/business-extractor/internal/domain"
)

const (
	earthRadiusM   = 6_371_008.8
	kmPerDegreeLat = 111.32
)

// HaversineM returns the great-circle distance in meters.
func HaversineM(a, b domain.Point) float64 {
	la1, la2 := a.Lat*math.Pi/180, b.Lat*math.Pi/180
	dLat := la2 - la1
	dLng := (b.Lng - a.Lng) * math.Pi / 180
	h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(la1)*math.Cos(la2)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * earthRadiusM * math.Asin(math.Min(1, math.Sqrt(h)))
}

// BBoxAround returns the box enclosing a circle.
func BBoxAround(c domain.Point, radiusM float64) domain.BBox {
	dLat := radiusM / 1000 / kmPerDegreeLat
	cos := math.Cos(c.Lat * math.Pi / 180)
	if cos < 1e-6 {
		cos = 1e-6
	}
	dLng := radiusM / 1000 / (kmPerDegreeLat * cos)
	return domain.BBox{
		MinLat: math.Max(-90, c.Lat-dLat), MaxLat: math.Min(90, c.Lat+dLat),
		MinLng: math.Max(-180, c.Lng-dLng), MaxLng: math.Min(180, c.Lng+dLng),
	}
}

// SizeKm returns the height and width of the box in kilometers.
func SizeKm(b domain.BBox) (h, w float64) {
	h = (b.MaxLat - b.MinLat) * kmPerDegreeLat
	cos := math.Cos(b.Center().Lat * math.Pi / 180)
	w = (b.MaxLng - b.MinLng) * kmPerDegreeLat * cos
	return h, w
}

// AutoCellKm picks a cell size so that the number of cells stays bounded
// (maxCells) while dense urban areas still get small cells.
func AutoCellKm(b domain.BBox, minKm float64, maxCells int) float64 {
	h, w := SizeKm(b)
	area := h * w
	if maxCells < 1 {
		maxCells = 1
	}
	size := math.Sqrt(area / float64(maxCells))
	if size < minKm {
		size = minKm
	}
	return size
}

// Grid splits b into cells of roughly cellKm x cellKm. The last row/column are
// clipped to the box. It never returns more than maxCells cells: if needed the
// cell size grows to respect the cap.
func Grid(b domain.BBox, cellKm float64, maxCells int) []domain.BBox {
	if !b.Valid() || cellKm <= 0 {
		return []domain.BBox{b}
	}
	h, w := SizeKm(b)
	rows := int(math.Ceil(h / cellKm))
	cols := int(math.Ceil(w / cellKm))
	if rows < 1 {
		rows = 1
	}
	if cols < 1 {
		cols = 1
	}
	for maxCells > 0 && rows*cols > maxCells {
		cellKm *= 1.25
		rows = max(1, int(math.Ceil(h/cellKm)))
		cols = max(1, int(math.Ceil(w/cellKm)))
	}
	dLat := (b.MaxLat - b.MinLat) / float64(rows)
	dLng := (b.MaxLng - b.MinLng) / float64(cols)
	cells := make([]domain.BBox, 0, rows*cols)
	// Serpentine order keeps consecutive tasks spatially close, which helps
	// upstream caches and keeps duplicates between neighbours in the same batch.
	for r := 0; r < rows; r++ {
		for i := 0; i < cols; i++ {
			c := i
			if r%2 == 1 {
				c = cols - 1 - i
			}
			cells = append(cells, domain.BBox{
				MinLat: b.MinLat + float64(r)*dLat,
				MaxLat: math.Min(b.MaxLat, b.MinLat+float64(r+1)*dLat),
				MinLng: b.MinLng + float64(c)*dLng,
				MaxLng: math.Min(b.MaxLng, b.MinLng+float64(c+1)*dLng),
			})
		}
	}
	return cells
}

// Split divides a cell in four, used when a cell saturates a source's result
// cap (adaptive quadtree refinement).
func Split(b domain.BBox) [4]domain.BBox {
	c := b.Center()
	return [4]domain.BBox{
		{MinLat: b.MinLat, MinLng: b.MinLng, MaxLat: c.Lat, MaxLng: c.Lng},
		{MinLat: b.MinLat, MinLng: c.Lng, MaxLat: c.Lat, MaxLng: b.MaxLng},
		{MinLat: c.Lat, MinLng: b.MinLng, MaxLat: b.MaxLat, MaxLng: c.Lng},
		{MinLat: c.Lat, MinLng: c.Lng, MaxLat: b.MaxLat, MaxLng: b.MaxLng},
	}
}

// ZoomForCell returns the Google Maps zoom level whose viewport roughly
// covers a cell of the given width.
func ZoomForCell(widthKm float64) int {
	if widthKm <= 0 {
		return 15
	}
	// At zoom z one 256px tile spans ~40075/2^z km at the equator; the Maps
	// viewport used by the HTTP search is ~600px wide.
	z := math.Log2(40075.0 * 600 / 256 / widthKm)
	return int(math.Max(3, math.Min(21, math.Round(z))))
}

const geohashAlphabet = "0123456789bcdefghjkmnpqrstuvwxyz"

// Geohash encodes a point with the given precision (1..12).
func Geohash(p domain.Point, precision int) string {
	if precision < 1 {
		precision = 1
	}
	if precision > 12 {
		precision = 12
	}
	latLo, latHi := -90.0, 90.0
	lngLo, lngHi := -180.0, 180.0
	out := make([]byte, precision)
	bit, ch, even := 0, 0, true
	for i := 0; i < precision; {
		if even {
			mid := (lngLo + lngHi) / 2
			if p.Lng >= mid {
				ch = ch<<1 | 1
				lngLo = mid
			} else {
				ch <<= 1
				lngHi = mid
			}
		} else {
			mid := (latLo + latHi) / 2
			if p.Lat >= mid {
				ch = ch<<1 | 1
				latLo = mid
			} else {
				ch <<= 1
				latHi = mid
			}
		}
		even = !even
		bit++
		if bit == 5 {
			out[i] = geohashAlphabet[ch]
			i++
			bit, ch = 0, 0
		}
	}
	return string(out)
}
