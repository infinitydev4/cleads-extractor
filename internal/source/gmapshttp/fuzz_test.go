package gmapshttp

import (
	"os"
	"testing"

	"github.com/cleads/business-extractor/internal/normalize"
)

func addSeeds(f *testing.F) {
	for _, p := range []string{"testdata/search_live.json", "testdata/preview_place_live.json", "testdata/place_raw.json"} {
		if b, err := os.ReadFile(p); err == nil {
			f.Add(b)
			f.Add(b[:len(b)/2])
		}
	}
	f.Add([]byte(searchPayload(3)))
}

// FuzzParse: the parsers must never panic, whatever Google returns.
func FuzzParse(f *testing.F) {
	addSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		items, _ := ParseSearch(b)
		for i := range items {
			normalize.Business(&items[i], "FR")
		}
		p, err := ParsePlace(b)
		if err == nil {
			normalize.Business(&p, "FR")
		}
	})
}
