package unionfs

import (
	"errors"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/absfs/absfs"
)

const (
	// WhiteoutPrefix starts the name of a whiteout marker. An empty file
	// named WhiteoutPrefix+name in a directory of the writable layer
	// deletes name from that directory in every layer below it.
	WhiteoutPrefix = ".wh."

	// OpaqueWhiteout is the name of the opaque marker. An empty file of this
	// name inside a directory of the writable layer makes the directory
	// opaque: none of the entries the same directory has in the layers
	// below show through it.
	OpaqueWhiteout = ".wh.__dir_opaque"

	// MetaPrefix starts the name of a metadata marker. An empty regular
	// file named MetaPrefix+name in a directory of the writable layer
	// records a change to the mode, times or owner of name, a regular file
	// that only the layers below have, without a copy of its contents: the
	// marker's own permission bits, modification time and owner are the
	// file's. See MetaMarkerName.
	MetaPrefix = WhiteoutPrefix + WhiteoutPrefix + "meta."

	// internalPrefix starts the names the union gives entries it is about
	// to delete or has replaced. Such a name is hidden like a whiteout but
	// deletes nothing; one is left behind only when a layer fails part way
	// through deleting it. Names starting with WhiteoutPrefix twice are
	// reserved for metadata, as in AUFS and OCI image layers.
	internalPrefix = WhiteoutPrefix + WhiteoutPrefix + "unionfs."
)

// defaultCopyBufferSize is the copy-up buffer size used unless
// WithCopyBufferSize sets another.
const defaultCopyBufferSize = 32 * 1024

var (
	// ErrNoWritableLayer is returned by every operation that changes the
	// union when it was built without WithWritableLayer.
	ErrNoWritableLayer = errors.New("no writable layer configured")

	// ErrReadOnlyLayer is not returned by this package: a union never
	// writes to its read-only layers. It is kept for compatibility.
	ErrReadOnlyLayer = errors.New("layer is read-only")
)

// Layer is one filesystem in a union's stack. Layers are added with
// WithWritableLayer and WithReadOnlyLayer.
type Layer struct {
	fs       absfs.FileSystem
	readOnly bool
}

// UnionFS merges a stack of filesystem layers into one view: an optional
// writable layer on top of any number of read-only layers. It implements
// absfs.SymlinkFileSystem, so it can be served or composed like any other
// absfs filesystem.
//
// A UnionFS is safe for concurrent use. Operations that change the union
// hold an exclusive lock while they resolve, copy up and write markers;
// lookups and listings share a read lock. Reads and writes through an open
// file handle do not take the lock.
type UnionFS struct {
	layers         []*Layer // ordered from top (highest precedence) to bottom
	writableLayer  *Layer   // the writable layer, layers[0], or nil
	mu             sync.RWMutex
	cache          *Cache
	lc             *lookupCache // nil unless WithLookupCache enables it
	copyBufferSize int
	asideSeq       uint64   // last suffix asideName used; guarded by mu
	spelled        *listing // the listing storedName read last; guarded by mu

	upperMarkersOnly bool // WithMarkersOnlyInWritableLayer

	cwdMu sync.RWMutex
	cwd   string
}

var _ absfs.SymlinkFileSystem = (*UnionFS)(nil)

// Option configures a UnionFS in New.
type Option func(*UnionFS)

// WithWritableLayer puts fs on top of the layer stack as the writable
// layer, the only layer a union ever writes to. Every change, including
// whiteouts and opaque markers, lands there; the other layers are never
// modified.
func WithWritableLayer(fs absfs.FileSystem) Option {
	return func(ufs *UnionFS) {
		layer := &Layer{fs: fs, readOnly: false}
		ufs.layers = append([]*Layer{layer}, ufs.layers...)
		ufs.writableLayer = layer
	}
}

// WithReadOnlyLayer adds fs below the layers added so far. Layers are
// searched in the order they were added, so the first read-only layer added
// takes precedence over the next.
func WithReadOnlyLayer(fs absfs.FileSystem) Option {
	return func(ufs *UnionFS) {
		ufs.layers = append(ufs.layers, &Layer{fs: fs, readOnly: true})
	}
}

// WithStatCache enables a cache of Stat and Lstat results with the given
// TTL; misses are cached for half as long. It holds at most 1000 results
// and 1000 misses. See WithCacheConfig, and WithLookupCache for a cache that
// never serves stale attributes.
func WithStatCache(enabled bool, ttl time.Duration) Option {
	return func(ufs *UnionFS) {
		ufs.cache = newCache(enabled, ttl, ttl/2, 1000)
	}
}

// WithCacheConfig enables a cache of Stat and Lstat results with separate
// TTLs for found and missing paths and a bound on entries per kind.
//
// A full cache evicts its oldest entry of the kind it stores, in constant
// time. Stat and Lstat results share the bound. A working set of paths
// larger than maxEntries, looked up in turn, misses on every call and
// costs about what no cache does; give such a workload a larger bound, or
// WithLookupCache, whose default bound is 100000 names.
//
// Changes made through the union drop the cached entries at and under the
// paths they touch. Changes made to a layer directly, writes through an open
// file handle (OpenFile drops the entry only when the file is opened, so a
// Stat between the open and a Write caches the old size), and changes seen
// through another path to the same file (a symbolic link) show only once the
// entry expires, or after InvalidateCache.
func WithCacheConfig(enabled bool, statTTL, negativeTTL time.Duration, maxEntries int) Option {
	return func(ufs *UnionFS) {
		ufs.cache = newCache(enabled, statTTL, negativeTTL, maxEntries)
	}
}

// WithLookupCache caches path resolution for ttl: which layer holds each
// name looked up, the whiteouts, opaque markers and metadata markers that
// apply to it, and which names are missing. Every lookup (Stat, Lstat,
// Open, ReadDir and the lookups inside every change) then reads only the
// final element's attributes from its layer, one call (two for a file a
// metadata marker changes), instead of probing every layer for every
// element of the path. A ttl of zero or less leaves it off, the default.
// maxEntries bounds the number of cached names; the cache is emptied when
// it fills. Zero or less means 100000.
//
// Unlike the Stat cache, it never serves attributes, so it stays exact
// under writes through open handles. Every change made through the union
// drops what it affects: the names it creates, removes, renames or copies
// up, with everything under them, and the names whose whiteouts, opaque
// markers or metadata markers it writes or removes. Changes made to a layer
// directly show only once the entries for them expire, or after
// InvalidateCache, InvalidateCacheTree or ClearCache: a name added to a
// layer may stay missing, or stay hidden by a layer above, and a name
// removed from a layer is found again in a lower layer only when the
// final element of a lookup goes missing.
//
// On a host that takes two spellings for one name ("d" and "D" where it
// folds case, NFC and NFD forms on macOS), a change made through one
// spelling drops only that spelling's entries, so the other may show the
// old view until it expires. A write never makes that old view permanent:
// a copy-up, or a new metadata marker, for an entry the cache resolved
// first checks it against the layers, and when the merged view has changed
// there (a lower file the other spelling's change whited out, or hid
// behind an opaque directory) the cache is emptied and the change fails,
// with ENOENT when the name is gone and ESTALE otherwise, so that a retry
// acts on the current view. A cached directory is never walked through
// once a link made through the union may have replaced it: creating a link
// in the writable layer, or renaming anything there but a regular file,
// empties the cache. A link made in the writable layer directly never
// carries a change out of it: every change there first checks, one Lstat
// per directory, that the directories above the name are still
// directories, and fails with ESTALE, emptying the cache, when one is not.
// The check is skipped for a layer basefs reports Confined. Reads are not
// checked, so until the entries expire they may follow such a link out of
// a layer that is not confined physically.
func WithLookupCache(ttl time.Duration, maxEntries int) Option {
	return func(ufs *UnionFS) {
		ufs.lc = nil
		if ttl > 0 {
			ufs.lc = newLookupCache(ttl, maxEntries)
		}
	}
}

// WithMarkersOnlyInWritableLayer makes the union read whiteouts, opaque
// markers, metadata markers and every other name in the reserved ".wh."
// space as markers only in the writable layer. In the read-only layers they
// are ordinary entries: a lower file or directory named ".wh.x" shows in the
// merged view, and can be read, listed and looked up, though not changed,
// removed or renamed, nor anything created inside it (EPERM), since the
// writable layer could not represent the result. Renaming a directory that
// holds such an entry, at any depth, fails with EPERM too, since its
// copied-up tree could not hold it; such a directory can be removed with
// RemoveAll, but not with Remove, which cannot empty it.
//
// By default markers count in every layer, as in AUFS and OCI image
// layers, so that a writable layer captured from one union can be stacked
// as a read-only layer of another and keep its deletions. The cost is that
// a read-only layer's own names starting with ".wh." vanish from the merged
// view without an error; ReservedNames finds them. Use this option for
// read-only layers that are plain trees (a source checkout, a fixture
// directory) rather than captured changesets: it stops ".wh." names
// vanishing, and stops whiteouts in those layers from hiding anything.
func WithMarkersOnlyInWritableLayer() Option {
	return func(ufs *UnionFS) {
		ufs.upperMarkersOnly = true
	}
}

// WithCopyBufferSize sets the size of the buffer used to copy a file up to
// the writable layer. The default is 32 KiB.
func WithCopyBufferSize(size int) Option {
	return func(ufs *UnionFS) {
		ufs.copyBufferSize = size
	}
}

// New returns a union of the layers the options add. Without
// WithWritableLayer the union is read-only and every change fails with
// ErrNoWritableLayer.
func New(opts ...Option) *UnionFS {
	ufs := &UnionFS{
		layers:         make([]*Layer, 0),
		copyBufferSize: defaultCopyBufferSize,
		cache:          newCache(false, 0, 0, 0),
		cwd:            "/",
	}
	for _, opt := range opts {
		opt(ufs)
	}
	return ufs
}

// Name returns "UnionFS".
func (ufs *UnionFS) Name() string {
	return "UnionFS"
}

// cleanPath normalizes a virtual filesystem path: forward slashes, clean,
// and absolute.
func cleanPath(p string) string {
	cleaned := path.Clean(p)
	if !strings.HasPrefix(cleaned, "/") {
		cleaned = "/" + cleaned
	}
	return cleaned
}

// InvalidateCache drops the cached Stat and Lstat results, or cached
// misses, for path, and the lookup cache's entries for path and everything
// under it. Use it after changing a layer directly rather than through the
// union.
func (ufs *UnionFS) InvalidateCache(path string) {
	path = cleanPath(path)
	ufs.cache.invalidate(path)
	if ufs.lc != nil {
		ufs.lc.invalidate(path)
	}
}

// InvalidateCacheTree drops every cached Stat and Lstat entry whose path
// starts with pathPrefix, and the lookup cache's entries for pathPrefix and
// everything under it.
func (ufs *UnionFS) InvalidateCacheTree(pathPrefix string) {
	pathPrefix = cleanPath(pathPrefix)
	ufs.cache.invalidateTree(pathPrefix)
	if ufs.lc != nil {
		ufs.lc.invalidate(pathPrefix)
	}
}

// ClearCache drops every cached entry, in the Stat cache and the lookup
// cache.
func (ufs *UnionFS) ClearCache() {
	ufs.cache.clear()
	if ufs.lc != nil {
		ufs.lc.clear()
	}
}

// CacheStats reports the configuration and size of the Stat cache and the
// lookup cache.
func (ufs *UnionFS) CacheStats() CacheStats {
	st := ufs.cache.Stats()
	if ufs.lc != nil {
		st.LookupEnabled, st.LookupEntries, st.LookupTTL = true, ufs.lc.len(), ufs.lc.ttl
	}
	return st
}
