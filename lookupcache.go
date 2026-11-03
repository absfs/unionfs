package unionfs

import (
	"sync"
	"time"
)

// defaultLookupEntries bounds the lookup cache when WithLookupCache is given
// no bound of its own.
const defaultLookupEntries = 100000

// lookupCache caches path resolution: for each path it has resolved, the
// node that says which layers hold the name and which markers apply to it,
// or that the name is missing. It caches structure, not attributes: a
// lookup served from it still reads the final element's info from its
// layer, so sizes and times are never stale.
//
// Entries form a tree mirroring the merged view, so dropping a path drops
// everything under it in one step. A cached node is shared by every lookup
// that reaches it; it is changed only under the union's exclusive lock, and
// only to record what the change it is part of has done.
type lookupCache struct {
	mu   sync.Mutex
	ttl  time.Duration
	max  int
	now  func() time.Time // the clock; tests replace it
	root *lcEntry         // "/", or nil
	size int              // entries below the root
}

// lcEntry is a cached path: its node, or nil for a name known to be
// missing, and the entries cached inside it when it is a directory.
type lcEntry struct {
	n       *node
	expires time.Time
	kids    map[string]*lcEntry
}

// count returns the number of entries in e's subtree, e included.
func (e *lcEntry) count() int {
	n := 1
	for _, k := range e.kids {
		n += k.count()
	}
	return n
}

func newLookupCache(ttl time.Duration, max int) *lookupCache {
	if max <= 0 {
		max = defaultLookupEntries
	}
	return &lookupCache{ttl: ttl, max: max, now: time.Now}
}

// live reports whether e is an entry that may still be served.
func (c *lookupCache) live(e *lcEntry, now time.Time) bool {
	return e != nil && now.Before(e.expires)
}

// dirEntry returns the live entry of directory node dir, or nil. It returns
// an entry only for the very node the cache holds: a node from before the
// path was dropped, or a copy, gets nothing.
func (c *lookupCache) dirEntry(dir *node, now time.Time) *lcEntry {
	e := c.root
	if !c.live(e, now) {
		return nil
	}
	p := dir.path
	for i := 1; i < len(p); {
		j := i
		for j < len(p) && p[j] != '/' {
			j++
		}
		e = e.kids[p[i:j]]
		if !c.live(e, now) || e.n == nil {
			return nil
		}
		i = j + 1
	}
	if e.n != dir {
		return nil
	}
	return e
}

// getRoot returns the cached root node, or nil.
func (c *lookupCache) getRoot() *node {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.live(c.root, c.now()) {
		return nil
	}
	return c.root.n
}

// putRoot caches the root node, dropping everything cached under it.
func (c *lookupCache) putRoot(n *node) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.root = &lcEntry{n: n, expires: c.now().Add(c.ttl)}
	c.size = 0
}

// get returns the cached result of looking name up in directory dir: the
// node, or nil for a missing name, and whether the cache had one.
func (c *lookupCache) get(dir *node, name string) (*node, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	d := c.dirEntry(dir, now)
	if d == nil {
		return nil, false
	}
	e := d.kids[name]
	if !c.live(e, now) {
		return nil, false
	}
	return e.n, true
}

// put caches n, or nil for a missing name, as the result of looking name
// up in directory dir, if dir is still the node the cache holds. When the
// cache is full it is emptied first.
func (c *lookupCache) put(dir *node, name string, n *node) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	d := c.dirEntry(dir, now)
	if d == nil {
		return
	}
	if c.size >= c.max {
		c.root.kids, c.size = nil, 0
		if d != c.root {
			return
		}
	}
	if d.kids == nil {
		d.kids = make(map[string]*lcEntry)
	}
	if old := d.kids[name]; old != nil {
		c.size -= old.count()
	}
	d.kids[name] = &lcEntry{n: n, expires: now.Add(c.ttl)}
	c.size++
}

// invalidate drops the cached entry for the clean absolute path p and
// everything under it. Dropping "/" empties the cache.
func (c *lookupCache) invalidate(p string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p == "/" || c.root == nil {
		c.root, c.size = nil, 0
		return
	}
	parent, e, name := (*lcEntry)(nil), c.root, ""
	for i := 1; i < len(p); {
		j := i
		for j < len(p) && p[j] != '/' {
			j++
		}
		parent, name, e = e, p[i:j], e.kids[p[i:j]]
		if e == nil {
			return
		}
		i = j + 1
	}
	delete(parent.kids, name)
	c.size -= e.count()
}

// clear empties the cache.
func (c *lookupCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.root, c.size = nil, 0
}

// len returns the number of entries cached below the root.
func (c *lookupCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.size
}
