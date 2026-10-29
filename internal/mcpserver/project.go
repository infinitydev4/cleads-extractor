package mcpserver

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/store"
)

// ---------------------------------------------------------------- schemas

// schemaOpts maps types the inference does not know to their JSON form.
func schemaOpts() *jsonschema.ForOptions {
	return &jsonschema.ForOptions{TypeSchemas: map[reflect.Type]*jsonschema.Schema{
		reflect.TypeFor[time.Time](): {Type: "string", Format: "date-time"},
	}}
}

// schemaFor infers the schema of T (structs are closed: unknown properties
// are rejected) and lets the caller add enums and bounds.
func schemaFor[T any](edit func(props map[string]*jsonschema.Schema)) *jsonschema.Schema {
	s, err := jsonschema.For[T](schemaOpts())
	if err != nil {
		panic(fmt.Sprintf("schema for %T: %v", *new(T), err))
	}
	if edit != nil {
		edit(s.Properties)
	}
	return s
}

// singleTyped rewrites the nullable types inferred for Go slices, maps and
// pointers (["null", "array"]) to their single type, recursively. Input
// schemas become function declarations for models, and some dialects (the
// OpenAPI subset used by Gemini, for one) accept only one type. Optional
// arguments are omitted, never sent as null. Output schemas keep null:
// the SDK validates results, where a nil slice encodes as null.
func singleTyped(s *jsonschema.Schema) {
	if s == nil {
		return
	}
	if len(s.Types) == 2 && slices.Contains(s.Types, "null") {
		for _, t := range s.Types {
			if t != "null" {
				s.Type, s.Types = t, nil
			}
		}
	}
	for _, p := range s.Properties {
		singleTyped(p)
	}
	singleTyped(s.Items)
}

func enum(p *jsonschema.Schema, vals ...string) {
	for _, v := range vals {
		p.Enum = append(p.Enum, v)
	}
}

func bounds(p *jsonschema.Schema, lo, hi float64) {
	p.Minimum, p.Maximum = &lo, &hi
}

// ---------------------------------------------------------------- filters

// Filters are the result filters shared by listing tools.
type Filters struct {
	MinCompleteness int    `json:"min_completeness,omitempty" jsonschema:"minimum completeness score 0-100 (identity, address, phone, website/email...)"`
	HasPhone        bool   `json:"has_phone,omitempty" jsonschema:"only records with a phone number"`
	HasEmail        bool   `json:"has_email,omitempty" jsonschema:"only records with at least one email (emails come from enrich: true jobs)"`
	HasWebsite      bool   `json:"has_website,omitempty" jsonschema:"only records with a website"`
	Useful          bool   `json:"useful,omitempty" jsonschema:"only CRM-useful records: operating and reachable by at least one channel"`
	City            string `json:"city,omitempty" jsonschema:"exact city name, case-insensitive"`
	PostalCode      string `json:"postal_code,omitempty" jsonschema:"postal code prefix, e.g. 69 or 69003"`
	Category        string `json:"category,omitempty" jsonschema:"category contains this text (in the job language)"`
	Q               string `json:"q,omitempty" jsonschema:"business name contains this text"`
}

func (f Filters) store() store.BusinessFilter {
	return store.BusinessFilter{MinCompleteness: f.MinCompleteness, HasPhone: f.HasPhone, HasEmail: f.HasEmail,
		HasWebsite: f.HasWebsite, OnlyUseful: f.Useful, City: strings.TrimSpace(f.City),
		PostalPrefix: strings.TrimSpace(f.PostalCode), Category: strings.TrimSpace(f.Category), Query: strings.TrimSpace(f.Q)}
}

// query encodes the filters as REST query parameters (sorted keys).
func (f Filters) query() url.Values {
	v := url.Values{}
	set := func(k, val string) {
		if val != "" && val != "false" && val != "0" {
			v.Set(k, val)
		}
	}
	set("min_completeness", strconv.Itoa(f.MinCompleteness))
	set("has_phone", strconv.FormatBool(f.HasPhone))
	set("has_email", strconv.FormatBool(f.HasEmail))
	set("has_website", strconv.FormatBool(f.HasWebsite))
	set("useful", strconv.FormatBool(f.Useful))
	set("city", f.City)
	set("postal_code", f.PostalCode)
	set("category", f.Category)
	set("q", f.Q)
	return v
}

// filtersFrom decodes filters encoded by query.
func filtersFrom(v url.Values) Filters {
	b := func(k string) bool { x, _ := strconv.ParseBool(v.Get(k)); return x }
	mc, _ := strconv.Atoi(v.Get("min_completeness"))
	return Filters{MinCompleteness: mc, HasPhone: b("has_phone"), HasEmail: b("has_email"), HasWebsite: b("has_website"),
		Useful: b("useful"), City: v.Get("city"), PostalCode: v.Get("postal_code"), Category: v.Get("category"), Q: v.Get("q")}
}

func (f *Filters) bound(p map[string]*jsonschema.Schema) {
	bounds(p["min_completeness"], 0, 100)
}

// ---------------------------------------------------------------- projection

// compactFields is the default projection: what a salesperson needs, without
// reviews, photos or raw identifiers, so pages stay small in the context.
var compactFields = []string{"id", "seq", "name", "category", "address", "postal_code", "city", "country", "phone",
	"website", "emails", "socials", "rating", "reviews_count", "status", "completeness"}

// businessFields lists every projectable field.
func businessFields() []string {
	t := reflect.TypeFor[domain.Business]()
	out := []string{"seq"}
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	return out
}

// projector selects fields of business records for an agent.
type projector struct {
	fields         []string
	includeReviews bool
}

func newProjector(fields []string, includeReviews bool) (*projector, error) {
	if len(fields) == 0 {
		fields = compactFields
	}
	all := businessFields()
	for _, f := range fields {
		if !slices.Contains(all, f) {
			return nil, fmt.Errorf("unknown field %q; available fields: %s", f, strings.Join(all, ", "))
		}
	}
	if includeReviews && !slices.Contains(fields, "details") {
		fields = append(slices.Clone(fields), "details")
	}
	return &projector{fields: fields, includeReviews: includeReviews}, nil
}

// project returns the selected fields of b. Details are trimmed: reviews
// (personal data of third parties, see docs/COMPLIANCE.md) only on request
// and capped, photos capped, popular times dropped.
func (p *projector) project(b domain.Business, seq int64) map[string]any {
	if b.Details != nil {
		d := *b.Details
		d.PopularTimes = nil
		if len(d.Images) > 3 {
			d.Images = d.Images[:3]
		}
		if p.includeReviews {
			rs := d.Reviews[:min(3, len(d.Reviews))]
			d.Reviews = make([]domain.Review, len(rs))
			for i, r := range rs {
				r.Text = clip(r.Text, 300)
				r.AuthorURL = ""
				d.Reviews[i] = r
			}
		} else {
			d.Reviews = nil
		}
		b.Details = &d
	}
	raw, _ := json.Marshal(b)
	var all map[string]any
	_ = json.Unmarshal(raw, &all)
	out := make(map[string]any, len(p.fields))
	for _, f := range p.fields {
		if f == "seq" {
			if seq > 0 {
				out["seq"] = seq
			}
			continue
		}
		if v, ok := all[f]; ok {
			out[f] = v
		}
	}
	return out
}

// fit keeps the leading items whose JSON encoding stays within budget bytes
// (at least one). It returns how many were kept.
func fit[T any](items []T, budget int) int {
	used := 0
	for i := range items {
		b, _ := json.Marshal(items[i])
		used += len(b) + 1
		if used > budget && i > 0 {
			return i
		}
	}
	return len(items)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
