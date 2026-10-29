package dedup

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/cleads/business-extractor/internal/domain"
)

func TestKeysOrderAndCrossSourceMatch(t *testing.T) {
	google := domain.Business{Name: "Dupont Plomberie", PlaceID: "ChIJ1", Phone: "+33478000000", Lat: 45.76, Lng: 4.83}
	sirene := domain.Business{Name: "SARL DUPONT PLOMBERIE", SIRET: "73282932000074", Lat: 45.7601, Lng: 4.8302}
	osm := domain.Business{Name: "Dupont plomberie", OSMID: "node/1", Phone: "+33478000000"}

	gk := Keys(&google)
	if !strings.HasPrefix(gk[0], "pid:") {
		t.Fatalf("strongest key must be the place id, got %v", gk)
	}
	shared := func(a, b []string) bool {
		for _, x := range a {
			for _, y := range b {
				if x == y {
					return true
				}
			}
		}
		return false
	}
	if !shared(gk, Keys(&sirene)) {
		t.Errorf("google and sirene records should share a name+geo key: %v / %v", gk, Keys(&sirene))
	}
	if !shared(gk, Keys(&osm)) {
		t.Errorf("google and osm records should share a name+phone key: %v / %v", gk, Keys(&osm))
	}
	other := domain.Business{Name: "Martin Electricité", Lat: 45.76, Lng: 4.83}
	if shared(gk, Keys(&other)) {
		t.Error("different businesses must not share keys")
	}
}

func TestCache(t *testing.T) {
	c := NewCache(1 << 12)
	if c.Contains("job|src", "pid:1") {
		t.Fatal("empty cache reports a hit")
	}
	c.Add("job|src", "pid:1")
	if !c.Contains("job|src", "pid:1") {
		t.Fatal("miss after add")
	}
	if c.Contains("other|src", "pid:1") {
		t.Fatal("scopes must be isolated")
	}
}

func TestCacheBounded(t *testing.T) {
	c := NewCache(1024)
	for i := 0; i < 100_000; i++ {
		c.Add("s", fmt.Sprint(i))
	}
	if n := c.Len(); n > 1024 {
		t.Fatalf("cache holds %d entries, cap is 1024", n)
	}
}

func TestCacheConcurrent(t *testing.T) {
	c := NewCache(1 << 16)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10_000; i++ {
				k := fmt.Sprint(i)
				if !c.Contains("s", k) {
					c.Add("s", k)
				}
			}
		}()
	}
	wg.Wait()
	if c.Len() != 10_000 {
		t.Fatalf("len = %d", c.Len())
	}
}

func BenchmarkKeys(b *testing.B) {
	biz := domain.Business{Name: "SARL Dupont Plomberie Chauffage", PlaceID: "ChIJabc", CID: "123", Phone: "+33478000000", Lat: 45.76, Lng: 4.83}
	for b.Loop() {
		_ = Keys(&biz)
	}
}

func BenchmarkCacheParallel(b *testing.B) {
	c := NewCache(1 << 20)
	keys := make([]string, 4096)
	for i := range keys {
		keys[i] = fmt.Sprintf("pid:ChIJ%08d", i)
	}
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			k := keys[i&4095]
			if !c.Contains("job|google_places", k) {
				c.Add("job|google_places", k)
			}
			i++
		}
	})
}
