// Package export streams businesses as CSV or JSON Lines. Writers never
// buffer the whole result set: memory stays flat whatever the export size.
package export

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/cleads/business-extractor/internal/domain"
)

// Writer writes one record at a time.
type Writer interface {
	Write(b *domain.Business) error
	Close() error
	ContentType() string
	Ext() string
}

// Columns of the CSV export, in order. Stable: CRM imports map on names.
var Columns = []string{
	"id", "name", "category", "phone", "email", "emails", "website", "domain",
	"address", "street", "postal_code", "city", "region", "country", "lat", "lng",
	"rating", "reviews_count", "status", "completeness", "siren", "siret", "naf",
	"linkedin", "facebook", "instagram", "place_id", "cid", "osm_id", "sources", "source_url", "hours",
}

// New returns a writer for format "csv", "csv-excel" (semicolon + BOM, for
// French Excel) or "jsonl".
func New(w io.Writer, format string) (Writer, error) {
	switch strings.ToLower(format) {
	case "", "csv":
		return newCSV(w, ',', false), nil
	case "csv-excel", "excel":
		return newCSV(w, ';', true), nil
	case "jsonl", "ndjson":
		bw := bufio.NewWriterSize(w, 64<<10)
		return &jsonlWriter{bw: bw, enc: json.NewEncoder(bw)}, nil
	}
	return nil, fmt.Errorf("unknown export format %q", format)
}

type csvWriter struct {
	bw  *bufio.Writer
	cw  *csv.Writer
	row []string
	hdr bool
}

func newCSV(w io.Writer, sep rune, bom bool) *csvWriter {
	bw := bufio.NewWriterSize(w, 64<<10)
	if bom {
		_, _ = bw.WriteString("\xEF\xBB\xBF") // UTF-8 BOM
	}
	cw := csv.NewWriter(bw)
	cw.Comma = sep
	return &csvWriter{bw: bw, cw: cw, row: make([]string, len(Columns))}
}

func (c *csvWriter) ContentType() string { return "text/csv; charset=utf-8" }
func (c *csvWriter) Ext() string         { return "csv" }

func (c *csvWriter) Write(b *domain.Business) error {
	if !c.hdr {
		c.hdr = true
		if err := c.cw.Write(Columns); err != nil {
			return err
		}
	}
	email := ""
	if len(b.Emails) > 0 {
		email = b.Emails[0]
	}
	f := func(v float64) string {
		if v == 0 {
			return ""
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	r := c.row
	r[0] = strconv.FormatInt(b.ID, 10)
	r[1], r[2], r[3], r[4] = safe(b.Name), safe(b.Category), safe(b.Phone), safe(email)
	r[5], r[6], r[7] = safe(strings.Join(b.Emails, " ")), safe(b.Website), safe(b.Domain)
	r[8], r[9], r[10], r[11], r[12], r[13] = safe(b.Address), safe(b.Street), safe(b.PostalCode), safe(b.City), safe(b.Region), b.Country
	r[14], r[15], r[16] = f(b.Lat), f(b.Lng), f(b.Rating)
	r[17] = ""
	if b.ReviewsCount > 0 {
		r[17] = strconv.Itoa(b.ReviewsCount)
	}
	r[18], r[19] = b.Status, strconv.Itoa(b.Completeness)
	r[20], r[21], r[22] = b.SIREN, b.SIRET, b.NAF
	r[23], r[24], r[25] = safe(b.Socials.LinkedIn), safe(b.Socials.Facebook), safe(b.Socials.Instagram)
	r[26], r[27], r[28] = b.PlaceID, b.CID, b.OSMID
	r[29], r[30] = strings.Join(b.Sources, " "), safe(b.SourceURL)
	r[31] = safe(hours(b.Hours))
	return c.cw.Write(r)
}

func (c *csvWriter) Close() error {
	if !c.hdr {
		_ = c.cw.Write(Columns)
	}
	c.cw.Flush()
	if err := c.cw.Error(); err != nil {
		return err
	}
	return c.bw.Flush()
}

// safe neutralizes CSV/formula injection: a cell starting with = + - @ is
// executed by spreadsheet software.
func safe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		// E.164 phone numbers are legitimate and safe: only digits follow.
		if s[0] == '+' && isDigits(s[1:]) {
			return s
		}
		return "'" + s
	}
	return s
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

func hours(h map[string]string) string {
	if len(h) == 0 {
		return ""
	}
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteString(" | ")
		}
		sb.WriteString(k)
		sb.WriteString(": ")
		sb.WriteString(h[k])
	}
	return sb.String()
}

type jsonlWriter struct {
	bw  *bufio.Writer
	enc *json.Encoder
}

func (j *jsonlWriter) ContentType() string            { return "application/x-ndjson" }
func (j *jsonlWriter) Ext() string                    { return "jsonl" }
func (j *jsonlWriter) Write(b *domain.Business) error { return j.enc.Encode(b) }
func (j *jsonlWriter) Close() error                   { return j.bw.Flush() }

// Flusher is implemented by writers that can push buffered bytes early.
type Flusher interface{ Flush() error }

func (c *csvWriter) Flush() error   { c.cw.Flush(); return c.bw.Flush() }
func (j *jsonlWriter) Flush() error { return j.bw.Flush() }
