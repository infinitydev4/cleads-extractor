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
	"fmt"
	"math/big"
	"net/url"
	"strconv"
	"strings"
	"time"

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

// ParseSearch parses a /search?tbm=map response body. A payload that does
// not match the expected layout yields an error, never a panic.
func ParseSearch(body []byte) (out []domain.Business, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("%w: parser panic: %v", errInvalidPayload, r)
		}
	}()
	return parseSearch(body)
}

func parseSearch(body []byte) ([]domain.Business, error) {
	body = stripXSSI(body)
	if !gjson.ValidBytes(body) {
		return nil, errInvalidPayload
	}
	items := gjson.GetBytes(body, "0.1")
	if !items.IsArray() {
		// Some responses wrap the payload in a JSON string under "d".
		if d := gjson.GetBytes(body, "d"); d.Type == gjson.String {
			return parseSearch([]byte(d.Str))
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

// ParsePlace parses a place payload (/maps/preview/place response or the
// APP_INITIALIZATION_STATE place entry). Never panics.
func ParsePlace(raw []byte) (b domain.Business, err error) {
	defer func() {
		if r := recover(); r != nil {
			b, err = domain.Business{}, fmt.Errorf("%w: parser panic: %v", errInvalidPayload, r)
		}
	}()
	return parsePlace(raw)
}

func parsePlace(raw []byte) (domain.Business, error) {
	raw = stripXSSI(raw)
	if !gjson.ValidBytes(raw) {
		return domain.Business{}, errInvalidPayload
	}
	biz := gjson.GetBytes(raw, "6")
	if !biz.IsArray() {
		return domain.Business{}, errInvalidPayload
	}
	f := scanFields(biz)
	b := businessFrom(f)
	if b.CID == "" {
		b.CID = gjson.GetBytes(raw, "25.3.0.13.0.0.1").Str
	}
	b.Details = parseDetails(f)
	b.DetailsAt = time.Now().UTC()
	return b, nil
}

// Top-level indexes of the business array that we read.
var wanted = [...]int{2, 4, 7, 9, 10, 11, 13, 18, 27, 30, 32, 34, 38, 39, 46, 57, 72, 75, 78, 84, 88, 100, 171, 175, 178, 183, 203}

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

func scanFields(d gjson.Result) *fields {
	// A single pass over the top-level array is cheaper than ~25 independent
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
	return &f
}

func parseBusiness(d gjson.Result) domain.Business { return businessFrom(scanFields(d)) }

func businessFrom(idx *fields) domain.Business {
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
		Status:    mapStatus(str(34, "4.4"), closedMarker(str(88, "0"))),
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
	b.DataID = idx.at(10).Str
	if b.Status == "" && b.Name != "" {
		// No closure marker: Maps only flags closed places.
		b.Status = domain.StatusOperational
	}
	return b
}

// parseDetails reads the place-page-only fields (description, attributes,
// photos, owner, reviews, popular times...).
func parseDetails(f *fields) *domain.Details {
	d := &domain.Details{
		Description: f.at(32).Get("1.1").Str,
		Thumbnail:   f.at(72).Get("0.1.6.0").Str,
		Timezone:    f.at(30).Str,
		PlusCode:    f.at(183).Get("2.2.0").Str,
		ReviewsLink: f.at(4).Get("3.0").Str,
	}
	if d.Description == "" && closedMarker(f.at(88).Get("0").Str) == "" {
		d.Description = f.at(88).Get("0").Str
	}
	if id, name := f.at(57).Get("2").Str, f.at(57).Get("1").Str; id != "" || name != "" {
		d.Owner = &domain.Owner{ID: id, Name: name}
		if id != "" {
			d.Owner.Link = "https://www.google.com/maps/contrib/" + id
		}
	}
	if u := f.at(38).Get("0").Str; u != "" {
		d.Menu = &domain.Link{URL: u, Title: f.at(38).Get("1").Str}
	}
	d.Reservations = links(f.at(46), "0", "1")
	order := f.at(75).Get("0.1.2")
	if !order.IsArray() {
		order = f.at(75).Get("0.0.2")
	}
	d.OrderOnline = links(order, "1.2.0", "0.0")
	d.Images = links(f.at(171).Get("0"), "3.0.6.0", "2")
	if len(d.Images) == 0 {
		// Places without a categorized gallery still expose their photo strip.
		d.Images = links(f.at(72).Get("0"), "6.0", "")
	}

	f.at(100).Get("1").ForEach(func(_, g gjson.Result) bool {
		grp := domain.AttributeGroup{ID: g.Get("0").Str, Name: g.Get("1").Str}
		g.Get("2").ForEach(func(_, o gjson.Result) bool {
			name := o.Get("1").Str
			if name == "" {
				return true
			}
			a := domain.Attribute{Name: name, Enabled: o.Get("2.1.0.0").Int() == 1}
			o.Get("2.4.1.0.0").ForEach(func(_, v gjson.Result) bool {
				if s := firstStr(v.Get("2"), v.Get("3")); s != "" {
					a.Values = append(a.Values, s)
				}
				return true
			})
			grp.Options = append(grp.Options, a)
			return true
		})
		if grp.Name != "" && len(grp.Options) > 0 {
			d.Attributes = append(d.Attributes, grp)
		}
		return true
	})

	if rpr := f.at(175).Get("3"); rpr.IsArray() {
		d.ReviewsPerRating = make(map[int]int, 5)
		for i := 0; i < 5; i++ {
			d.ReviewsPerRating[i+1] = int(rpr.Get(strconv.Itoa(i)).Int())
		}
	}
	revs := f.at(175).Get("9.0.0")
	if !revs.IsArray() {
		revs = f.at(175).Get("9.0")
	}
	revs.ForEach(func(_, it gjson.Result) bool {
		el := it.Get("0")
		if !el.IsArray() {
			el = it
		}
		r := domain.Review{
			ID:        el.Get("0").Str,
			Author:    firstStr(el.Get("1.4.5.0"), el.Get("1.4.4"), el.Get("0.1")),
			AuthorURL: el.Get("1.4.2.0").Str,
			Rating:    int(firstNum(el.Get("2.0.0"), el.Get("2.0"), el.Get("1.0.0"))),
			Text:      firstStr(el.Get("2.15.0.0"), el.Get("2.15.0"), el.Get("3.0")),
			When:      firstStr(el.Get("1.6"), el.Get("3.3")),
			Language:  el.Get("2.14.0").Str,
			Reply:     firstStr(el.Get("3.14.0.0"), el.Get("3.14.1.0")),
		}
		if us := el.Get("1.2").Int(); us > 1e15 {
			r.PublishedAt = time.UnixMicro(us).UTC()
		}
		if r.Author != "" {
			d.Reviews = append(d.Reviews, r)
		}
		return true
	})

	days := map[int64]string{1: "Monday", 2: "Tuesday", 3: "Wednesday", 4: "Thursday", 5: "Friday", 6: "Saturday", 7: "Sunday"}
	f.at(84).Get("0").ForEach(func(_, day gjson.Result) bool {
		name, ok := days[day.Get("0").Int()]
		if !ok {
			return true
		}
		hours := map[int]int{}
		day.Get("1").ForEach(func(_, h gjson.Result) bool {
			hours[int(h.Get("0").Int())] = int(h.Get("1").Int())
			return true
		})
		if len(hours) > 0 {
			if d.PopularTimes == nil {
				d.PopularTimes = map[string]map[int]int{}
			}
			d.PopularTimes[name] = hours
		}
		return true
	})
	return d
}

func links(arr gjson.Result, linkPath, titlePath string) []domain.Link {
	var out []domain.Link
	arr.ForEach(func(_, it gjson.Result) bool {
		if u := it.Get(linkPath).Str; u != "" {
			l := domain.Link{URL: u}
			if titlePath != "" {
				l.Title = it.Get(titlePath).Str
			}
			out = append(out, l)
		}
		return true
	})
	return out
}

func firstStr(rs ...gjson.Result) string {
	for _, r := range rs {
		if r.Type == gjson.String && r.Str != "" {
			return r.Str
		}
	}
	return ""
}

func firstNum(rs ...gjson.Result) float64 {
	for _, r := range rs {
		if r.Type == gjson.Number && r.Num != 0 {
			return r.Num
		}
	}
	return 0
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

// closedMarker returns [88][0] when it is the localized closure enum
// ("FERMÉ", "CLOSED", "GESCHLOSSEN"...): a short upper-case label. Otherwise
// that slot holds the editorial description and is ignored here.
func closedMarker(v string) string {
	if v == "" || len(v) > 40 || strings.ToUpper(v) != v || strings.ToLower(v) == v {
		return ""
	}
	return v
}

func mapStatus(legacy, closed string) string {
	l := strings.ToLower(legacy + " " + closed)
	switch {
	case strings.TrimSpace(l) == "":
		return ""
	case strings.Contains(l, "tempor"):
		return domain.StatusClosedTemporarily
	case closed != "" || strings.Contains(l, "permanently") || strings.Contains(l, "définitivement"):
		return domain.StatusClosedPermanently
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
