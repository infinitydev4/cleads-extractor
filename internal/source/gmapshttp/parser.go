package gmapshttp

// Parsing of Google Maps internal JSON arrays.
//
// The positional paths (e.g. business[11] = title) were reverse-engineered by
// gosom/google-maps-scraper (MIT License, Copyright (c) 2023 Georgios
// Komninos). This file reimplements them on top of gjson so that fields are
// read lazily from the raw bytes instead of materialising the whole document
// as []any: on a 170 KB place payload this divides allocations by ~50 (see
// BenchmarkParsePlace*).

import (
	"bytes"
	"math/big"
	"net/url"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/cleads/business-extractor/internal/domain"
)

var xssiPrefix = []byte(")]}'")

// stripXSSI removes Google's anti-JSON-hijacking prefix line.
func stripXSSI(b []byte) []byte {
	b = bytes.TrimSpace(b)
	if bytes.HasPrefix(b, xssiPrefix) {
		b = b[len(xssiPrefix):]
		if i := bytes.IndexByte(b, '\n'); i >= 0 && i < 4 {
			b = b[i+1:]
		}
	}
	return bytes.TrimSpace(b)
}

// ParseSearch parses a /search?tbm=map response body.
func ParseSearch(body []byte) ([]domain.Business, error) {
	body = stripXSSI(body)
	if !gjson.ValidBytes(body) {
		return nil, errInvalidPayload
	}
	items := gjson.GetBytes(body, "0.1")
	if !items.IsArray() {
		// Some responses wrap the payload in a JSON string under "d".
		if d := gjson.GetBytes(body, "d"); d.Type == gjson.String {
			return ParseSearch([]byte(d.Str))
		}
		return nil, errInvalidPayload
	}
	arr := items.Array()
	out := make([]domain.Business, 0, len(arr))
	for i, it := range arr {
		if i == 0 {
			continue // first element is search metadata
		}
		biz := it.Get("14")
		if !biz.IsArray() {
			continue
		}
		b := parseBusiness(biz)
		if b.Name == "" {
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

// ParsePlace parses the APP_INITIALIZATION_STATE place payload (place page).
func ParsePlace(raw []byte) (domain.Business, error) {
	raw = stripXSSI(raw)
	if !gjson.ValidBytes(raw) {
		return domain.Business{}, errInvalidPayload
	}
	biz := gjson.GetBytes(raw, "6")
	if !biz.IsArray() {
		return domain.Business{}, errInvalidPayload
	}
	b := parseBusiness(biz)
	if b.CID == "" {
		b.CID = gjson.GetBytes(raw, "25.3.0.13.0.0.1").Str
	}
	return b, nil
}

// Top-level indexes of the business array that we read.
var wanted = [...]int{2, 4, 7, 9, 10, 11, 13, 18, 27, 34, 39, 78, 88, 178, 183, 203}

const maxWanted = 203

var slotOf = func() (t [maxWanted + 1]int8) {
	for i := range t {
		t[i] = -1
	}
	for s, n := range wanted {
		t[n] = int8(s)
	}
	return t
}()

type fields [len(wanted)]gjson.Result

func (f *fields) at(n int) gjson.Result { return f[slotOf[n]] }

func parseBusiness(d gjson.Result) domain.Business {
	// A single pass over the top-level array is cheaper than ~20 independent
	// path lookups that would each rescan the payload from the start.
	var f fields
	i := 0
	d.ForEach(func(_, v gjson.Result) bool {
		if s := slotOf[i]; s >= 0 {
			f[s] = v
		}
		i++
		return i <= maxWanted
	})
	idx := &f
	str := func(n int, path string) string {
		if path == "" {
			return idx.at(n).Str
		}
		return idx.at(n).Get(path).Str
	}

	b := domain.Business{
		Name:      str(11, ""),
		Website:   actualURL(str(7, "0")),
		Phone:     strings.ReplaceAll(str(178, "0.0"), " ", ""),
		Status:    mapStatus(str(34, "4.4"), str(88, "0")),
		PlaceID:   str(78, ""),
		Sources:   []string{Name},
		SourceURL: str(27, ""),
	}
	if cats := idx.at(13); cats.IsArray() {
		cats.ForEach(func(_, c gjson.Result) bool {
			if c.Str != "" {
				b.Categories = append(b.Categories, c.Str)
			}
			return true
		})
		if len(b.Categories) > 0 {
			b.Category = b.Categories[0]
		}
	}
	b.Rating = idx.at(4).Get("7").Float()
	b.ReviewsCount = int(idx.at(4).Get("8").Int())
	b.PriceLevel = idx.at(4).Get("2").Str
	b.Lat = idx.at(9).Get("2").Float()
	b.Lng = idx.at(9).Get("3").Float()

	// Address: [39] is the full address on recent payloads, [18] is
	// "Title, address", [2] is the address split in lines.
	if a := idx.at(39).Str; a != "" {
		b.Address = a
	} else if a := idx.at(18).Str; a != "" {
		b.Address = strings.TrimSpace(strings.TrimPrefix(a, b.Name+","))
	} else if parts := idx.at(2); parts.IsArray() {
		var sb strings.Builder
		parts.ForEach(func(_, p gjson.Result) bool {
			if sb.Len() > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(p.String())
			return true
		})
		b.Address = sb.String()
	}
	if ca := idx.at(183).Get("1"); ca.IsArray() {
		b.Street = ca.Get("1").Str
		b.City = ca.Get("3").Str
		b.PostalCode = ca.Get("4").Str
		b.Region = ca.Get("5").Str
	}

	if dataID := idx.at(10).Str; dataID != "" {
		b.CID = cidFromDataID(dataID)
	}
	if b.SourceURL == "" && b.PlaceID != "" {
		b.SourceURL = "https://www.google.com/maps/place/?q=place_id:" + b.PlaceID
	}
	b.Hours = parseHours(idx.at(203).Get("0"))
	if len(b.Hours) == 0 {
		b.Hours = parseHours(idx.at(34).Get("1"))
	}
	return b
}

// parseHours reads [[day, _, _, [[ "9:00–18:00", ...], ...]], ...].
func parseHours(items gjson.Result) map[string]string {
	if !items.IsArray() {
		return nil
	}
	out := make(map[string]string, 7)
	items.ForEach(func(_, it gjson.Result) bool {
		day := it.Get("0").Str
		if day == "" {
			return true
		}
		var slots []string
		if s := it.Get("3"); s.IsArray() {
			s.ForEach(func(_, slot gjson.Result) bool {
				if t := slot.Get("0").Str; t != "" {
					slots = append(slots, t)
				}
				return true
			})
		} else if s := it.Get("1"); s.IsArray() {
			s.ForEach(func(_, slot gjson.Result) bool {
				if slot.Str != "" {
					slots = append(slots, slot.Str)
				}
				return true
			})
		}
		if len(slots) > 0 {
			out[day] = strings.Join(slots, ", ")
		}
		return true
	})
	if len(out) == 0 {
		return nil
	}
	return out
}

func mapStatus(legacy, closed string) string {
	l := strings.ToLower(legacy + " " + closed)
	switch {
	case strings.Contains(l, "permanently") || strings.Contains(l, "définitivement") || closed == "CLOSED":
		return domain.StatusClosedPermanently
	case strings.Contains(l, "temporarily") || strings.Contains(l, "temporairement"):
		return domain.StatusClosedTemporarily
	case strings.TrimSpace(l) == "":
		return ""
	}
	return domain.StatusOperational
}

// cidFromDataID converts "0x47e66e1f06e2b70f:0x40b82c3688c9460" to the
// decimal CID (the second half).
func cidFromDataID(dataID string) string {
	_, h, ok := strings.Cut(dataID, ":")
	if !ok {
		return ""
	}
	h = strings.TrimPrefix(strings.ToLower(h), "0x")
	n, ok := new(big.Int).SetString(h, 16)
	if !ok || n.Sign() == 0 {
		return ""
	}
	return n.String()
}

// actualURL unwraps Google redirect links ("/url?q=https://...").
func actualURL(u string) string {
	if !strings.HasPrefix(u, "/url?") {
		return u
	}
	p, err := url.Parse(u)
	if err != nil {
		return u
	}
	if q := p.Query().Get("q"); q != "" {
		return q
	}
	return u
}
