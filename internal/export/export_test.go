package export

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cleads/business-extractor/internal/domain"
)

func TestCSV(t *testing.T) {
	var buf bytes.Buffer
	w, _ := New(&buf, "csv")
	b := domain.Business{ID: 7, Name: "=HYPERLINK(\"http://evil\")", Phone: "+33478000000", Emails: []string{"a@x.fr", "b@x.fr"},
		Hours: map[string]string{"mardi": "9-18", "lundi": "9-18"}}
	if err := w.Write(&b); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || len(rows[0]) != len(Columns) {
		t.Fatalf("rows = %v", rows)
	}
	r := rows[1]
	if !strings.HasPrefix(r[1], "'=") {
		t.Errorf("formula injection not neutralized: %q", r[1])
	}
	if r[3] != "+33478000000" {
		t.Errorf("E.164 phone must not be escaped: %q", r[3])
	}
	if r[4] != "a@x.fr" || r[5] != "a@x.fr b@x.fr" {
		t.Errorf("emails: %q %q", r[4], r[5])
	}
	if r[31] != "lundi: 9-18 | mardi: 9-18" {
		t.Errorf("hours: %q", r[31])
	}
}

func TestExcelVariant(t *testing.T) {
	var buf bytes.Buffer
	w, _ := New(&buf, "csv-excel")
	_ = w.Write(&domain.Business{Name: "A"})
	_ = w.Close()
	if !bytes.HasPrefix(buf.Bytes(), []byte("\xEF\xBB\xBF")) || !strings.Contains(buf.String(), "id;name;") {
		t.Fatalf("excel csv: %q", buf.String()[:40])
	}
}

func TestJSONL(t *testing.T) {
	var buf bytes.Buffer
	w, _ := New(&buf, "jsonl")
	_ = w.Write(&domain.Business{Name: "A"})
	_ = w.Write(&domain.Business{Name: "B"})
	_ = w.Close()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatal(lines)
	}
	var b domain.Business
	if err := json.Unmarshal([]byte(lines[1]), &b); err != nil || b.Name != "B" {
		t.Fatal(err, b)
	}
}

func TestEmptyCSVHasHeader(t *testing.T) {
	var buf bytes.Buffer
	w, _ := New(&buf, "csv")
	_ = w.Close()
	if !strings.HasPrefix(buf.String(), "id,name,") {
		t.Fatal(buf.String())
	}
}

func BenchmarkCSVWrite(b *testing.B) {
	w, _ := New(&bytes.Buffer{}, "csv")
	biz := domain.Business{ID: 1, Name: "Dupont Plomberie", Phone: "+33478000000", Website: "https://dupont.fr",
		Address: "1 Rue X, 69003 Lyon", City: "Lyon", PostalCode: "69003", Emails: []string{"contact@dupont.fr"}, Sources: []string{"google_places"}}
	b.ReportAllocs()
	for b.Loop() {
		_ = w.Write(&biz)
	}
}
