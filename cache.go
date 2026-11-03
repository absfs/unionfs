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
}

// statCacheEntry stores cached file info
type statCacheEntry struct {
	info    os.FileInfo
	layer   int
	expires time.Time
}

// negativeCacheEntry stores information about non-existent paths
type negativeCacheEntry struct {
	expires time.Time
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

	// Evict old entries if cache is full
	if len(c.statCache) >= c.maxEntries {
		c.evictOldestStat()
	}

	c.statCache[path] = &statCacheEntry{
		info:    info,
		layer:   layer,
		expires: c.now().Add(c.statTTL),
	}
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

	// Evict old entries if cache is full
	if len(c.negativeCache) >= c.maxEntries {
		c.evictOldestNegative()
	}

	c.negativeCache[path] = &negativeCacheEntry{
		expires: c.now().Add(c.negativeTTL),
	}
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
}

// evictOldestStat removes the oldest stat cache entry
func (c *Cache) evictOldestStat() {
	var oldestPath string
	var oldestTime time.Time

	for path, entry := range c.statCache {
		if oldestPath == "" || entry.expires.Before(oldestTime) {
			oldestPath = path
			oldestTime = entry.expires
		}
	}

	if oldestPath != "" {
		delete(c.statCache, oldestPath)
	}
}

// evictOldestNegative removes the oldest negative cache entry
func (c *Cache) evictOldestNegative() {
	var oldestPath string
	var oldestTime time.Time

	for path, entry := range c.negativeCache {
		if oldestPath == "" || entry.expires.Before(oldestTime) {
			oldestPath = path
			oldestTime = entry.expires
		}
	}

	if oldestPath != "" {
		delete(c.negativeCache, oldestPath)
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
