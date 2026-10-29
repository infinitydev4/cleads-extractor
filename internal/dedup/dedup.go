// Package dedup implements entity resolution for business records.
//
// Each record gets a list of blocking keys ordered from strongest (source
// identifiers) to weakest (fuzzy name + location). Two records sharing any key
// are considered the same business. The store resolves keys transactionally;
// this package also provides a bounded in-process cache that drops the bulk of
// repeated hits (overlapping grid cells) before they reach the database.
package dedup

import (
	"hash/fnv"
	"hash/maphash"
	"sync"
	"sync/atomic"

	"github.com/cleads/business-extractor/internal/domain"
	"github.com/cleads/business-extractor/internal/geo"
	"github.com/cleads/business-extractor/internal/normalize"
)

// Keys returns the blocking keys of b, strongest first. b must already be
// normalized (E.164 phone, canonical domain).
func Keys(b *domain.Business) []string {
	keys := make([]string, 0, 6)
	if b.PlaceID != "" {
		keys = append(keys, "pid:"+b.PlaceID)
	}
	if b.CID != "" {
		keys = append(keys, "cid:"+b.CID)
	}
	if b.SIRET != "" {
		keys = append(keys, "siret:"+b.SIRET)
	}
	if b.OSMID != "" {
		keys = append(keys, "osm:"+b.OSMID)
	}
	nk := normalize.NameKey(b.Name)
	if nk != "" {
		if b.Phone != "" {
			keys = append(keys, "np:"+nk+"|"+b.Phone)
		}
		if b.Lat != 0 || b.Lng != 0 {
			// geohash6 cells are ~1.2km x 0.6km: same name that close is the same shop.
			keys = append(keys, "ng:"+nk+"|"+geo.Geohash(domain.Point{Lat: b.Lat, Lng: b.Lng}, 6))
		} else if b.PostalCode != "" {
			keys = append(keys, "na:"+nk+"|"+b.PostalCode)
		} else if b.Domain != "" {
			keys = append(keys, "nd:"+nk+"|"+b.Domain)
		}
	}
	return keys
}

// StableHash is a process-independent 64-bit hash, used for advisory locks.
func StableHash(s string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return int64(h.Sum64())
}

const shards = 64

// Cache is a sharded, bounded set of hashed keys. It is a best-effort filter:
// when a shard is full it is reset, so false negatives are possible but false
// positives are not (up to 64-bit hash collisions).
type Cache struct {
	seed     maphash.Seed
	perShard int
	shards   [shards]struct {
		mu  sync.Mutex
		set map[uint64]struct{}
	}
	hits, misses atomic.Int64
}

// NewCache creates a cache bounded to roughly capacity entries.
func NewCache(capacity int) *Cache {
	if capacity < shards {
		capacity = shards
	}
	c := &Cache{seed: maphash.MakeSeed(), perShard: capacity / shards}
	for i := range c.shards {
		c.shards[i].set = make(map[uint64]struct{}, 1024)
	}
	return c
}

func (c *Cache) hash(scope, key string) uint64 {
	var h maphash.Hash
	h.SetSeed(c.seed)
	_, _ = h.WriteString(scope)
	_ = h.WriteByte(0)
	_, _ = h.WriteString(key)
	return h.Sum64()
}

// Contains reports whether (scope, key) was recorded.
func (c *Cache) Contains(scope, key string) bool {
	k := c.hash(scope, key)
	s := &c.shards[k%shards]
	s.mu.Lock()
	_, ok := s.set[k]
	s.mu.Unlock()
	if ok {
		c.hits.Add(1)
	} else {
		c.misses.Add(1)
	}
	return ok
}

// Add records (scope, key). Call it only once the record is persisted, so a
// failed write is retried in full.
func (c *Cache) Add(scope, key string) {
	k := c.hash(scope, key)
	s := &c.shards[k%shards]
	s.mu.Lock()
	if len(s.set) >= c.perShard {
		clear(s.set)
	}
	s.set[k] = struct{}{}
	s.mu.Unlock()
}

// Stats returns cache hit/miss counters.
func (c *Cache) Stats() (hits, misses int64) { return c.hits.Load(), c.misses.Load() }

// Len returns the number of entries currently held.
func (c *Cache) Len() int {
	n := 0
	for i := range c.shards {
		c.shards[i].mu.Lock()
		n += len(c.shards[i].set)
		c.shards[i].mu.Unlock()
	}
	return n
}
