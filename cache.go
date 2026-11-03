package unionfs

import (
	"os"
	"sync"
	"time"
)

// Cache is the Stat and Lstat cache a UnionFS keeps when WithStatCache or
// WithCacheConfig enables it. Callers never construct or receive one: it is
// exported only for compatibility with v1.0.0. Use UnionFS.CacheStats,
// InvalidateCache, InvalidateCacheTree and ClearCache instead.
type Cache struct {
	statCache     map[string]*statCacheEntry
	negativeCache map[string]*negativeCacheEntry
	mu            sync.RWMutex
	statTTL       time.Duration
	negativeTTL   time.Duration
	maxEntries    int
	enabled       bool
	now           func() time.Time // the clock; tests replace it

	// statOrder and negativeOrder record the keys of each map in the
	// order they were stored, so that a full map evicts its oldest entry
	// without scanning the map.
	statOrder     evictQueue
	negativeOrder evictQueue
}

// statCacheEntry stores cached file info
type statCacheEntry struct {
	info    os.FileInfo
	layer   int
	expires time.Time
	seq     uint64 // its place in statOrder
}

// negativeCacheEntry stores information about non-existent paths
type negativeCacheEntry struct {
	expires time.Time
	seq     uint64 // its place in negativeOrder
}

// evictQueue lists the keys stored in a map, oldest first. Every entry of
// a map has the same lifetime, so the oldest stored is the first to expire.
// A key stored again, or deleted, leaves a stale record behind: each record
// carries the sequence number its entry was stored with, and a record whose
// entry has another (or none) is skipped. Eviction pops records, and the
// list is compacted once stale records outnumber the map, so storing an
// entry costs amortized constant time however full the map is.
type evictQueue struct {
	keys []queued
	seq  uint64
}

type queued struct {
	key string
	seq uint64
}

// push records key as stored now and returns its sequence number.
func (q *evictQueue) push(key string) uint64 {
	q.seq++
	q.keys = append(q.keys, queued{key: key, seq: q.seq})
	return q.seq
}

// pop removes and returns the oldest key whose record live accepts.
func (q *evictQueue) pop(live func(key string, seq uint64) bool) (string, bool) {
	for len(q.keys) > 0 {
		k := q.keys[0]
		q.keys = q.keys[1:]
		if live(k.key, k.seq) {
			return k.key, true
		}
	}
	return "", false
}

// compact drops stale records once the list holds more than twice size of
// them, size being the map's length.
func (q *evictQueue) compact(size int, live func(key string, seq uint64) bool) {
	if len(q.keys) <= 2*size+16 {
		return
	}
	kept := make([]queued, 0, size)
	for _, k := range q.keys {
		if live(k.key, k.seq) {
			kept = append(kept, k)
		}
	}
	q.keys = kept
}

// reset empties the list.
func (q *evictQueue) reset() { q.keys = nil }

func (c *Cache) statLive(key string, seq uint64) bool {
	e, ok := c.statCache[key]
	return ok && e.seq == seq
}

func (c *Cache) negativeLive(key string, seq uint64) bool {
	e, ok := c.negativeCache[key]
	return ok && e.seq == seq
}

// newCache creates a new cache with the specified configuration
func newCache(enabled bool, statTTL, negativeTTL time.Duration, maxEntries int) *Cache {
	if !enabled {
		return &Cache{enabled: false}
	}

	return &Cache{
		statCache:     make(map[string]*statCacheEntry),
		negativeCache: make(map[string]*negativeCacheEntry),
		statTTL:       statTTL,
		negativeTTL:   negativeTTL,
		maxEntries:    maxEntries,
		enabled:       true,
		now:           time.Now,
	}
}

// getStat retrieves a cached stat entry if available and not expired
func (c *Cache) getStat(path string) (os.FileInfo, int, bool) {
	if !c.enabled {
		return nil, -1, false
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, ok := c.statCache[path]
	if !ok {
		return nil, -1, false
	}

	// An entry is served only before it expires, so a zero TTL never
	// serves one, however coarse the clock.
	if !c.now().Before(entry.expires) {
		return nil, -1, false
	}

	return entry.info, entry.layer, true
}

// putStat stores a stat result in the cache
func (c *Cache) putStat(path string, info os.FileInfo, layer int) {
	if !c.enabled {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Evict the oldest entry if the cache is full
	if _, ok := c.statCache[path]; !ok && len(c.statCache) >= c.maxEntries {
		c.evictOldestStat()
	}

	c.statCache[path] = &statCacheEntry{
		info:    info,
		layer:   layer,
		expires: c.now().Add(c.statTTL),
		seq:     c.statOrder.push(path),
	}
	c.statOrder.compact(len(c.statCache), c.statLive)
}

// isNegative checks if a path is in the negative cache (known not to exist)
func (c *Cache) isNegative(path string) bool {
	if !c.enabled {
		return false
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, ok := c.negativeCache[path]
	if !ok {
		return false
	}

	// An entry is served only before it expires, so a zero TTL never
	// serves one, however coarse the clock.
	if !c.now().Before(entry.expires) {
		return false
	}

	return true
}

// putNegative marks a path as non-existent in the cache
func (c *Cache) putNegative(path string) {
	if !c.enabled {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Evict the oldest entry if the cache is full
	if _, ok := c.negativeCache[path]; !ok && len(c.negativeCache) >= c.maxEntries {
		c.evictOldestNegative()
	}

	c.negativeCache[path] = &negativeCacheEntry{
		expires: c.now().Add(c.negativeTTL),
		seq:     c.negativeOrder.push(path),
	}
	c.negativeOrder.compact(len(c.negativeCache), c.negativeLive)
}

// lstatKey is appended to a path to key its Lstat result, so that the
// Lstat entries of a tree start with the tree's path as its Stat entries do.
const lstatKey = "\x00lstat"

// invalidate removes a path's Stat and Lstat entries from all caches
func (c *Cache) invalidate(path string) {
	if !c.enabled {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	for _, k := range []string{path, path + lstatKey} {
		delete(c.statCache, k)
		delete(c.negativeCache, k)
	}
}

// invalidateTree removes all cache entries under a given path prefix
func (c *Cache) invalidateTree(pathPrefix string) {
	if !c.enabled {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Remove all entries that start with the path prefix
	for path := range c.statCache {
		if len(path) >= len(pathPrefix) && path[:len(pathPrefix)] == pathPrefix {
			delete(c.statCache, path)
		}
	}

	for path := range c.negativeCache {
		if len(path) >= len(pathPrefix) && path[:len(pathPrefix)] == pathPrefix {
			delete(c.negativeCache, path)
		}
	}
}

// clear removes all cache entries
func (c *Cache) clear() {
	if !c.enabled {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.statCache = make(map[string]*statCacheEntry)
	c.negativeCache = make(map[string]*negativeCacheEntry)
	c.statOrder.reset()
	c.negativeOrder.reset()
}

// evictOldestStat removes the oldest stat cache entry
func (c *Cache) evictOldestStat() {
	if k, ok := c.statOrder.pop(c.statLive); ok {
		delete(c.statCache, k)
	}
}

// evictOldestNegative removes the oldest negative cache entry
func (c *Cache) evictOldestNegative() {
	if k, ok := c.negativeOrder.pop(c.negativeLive); ok {
		delete(c.negativeCache, k)
	}
}

// Stats returns the cache's configuration and current size.
func (c *Cache) Stats() CacheStats {
	if !c.enabled {
		return CacheStats{Enabled: false}
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	return CacheStats{
		Enabled:           true,
		StatCacheSize:     len(c.statCache),
		NegativeCacheSize: len(c.negativeCache),
		MaxEntries:        c.maxEntries,
		StatTTL:           c.statTTL,
		NegativeTTL:       c.negativeTTL,
	}
}

// CacheStats describes a union's Stat cache and lookup cache, as
// UnionFS.CacheStats reports them. The Stat cache's fields are zero when it
// is disabled, and the lookup cache's when it is.
type CacheStats struct {
	Enabled           bool          // whether the Stat cache is on
	StatCacheSize     int           // cached Stat and Lstat results, live or expired
	NegativeCacheSize int           // cached misses, live or expired
	MaxEntries        int           // bound on entries of each kind
	StatTTL           time.Duration // how long a Stat result is served
	NegativeTTL       time.Duration // how long a miss is served

	LookupEnabled bool          // whether the lookup cache is on (WithLookupCache)
	LookupEntries int           // names it holds, found or missing, live or expired
	LookupTTL     time.Duration // how long it serves a name
}
