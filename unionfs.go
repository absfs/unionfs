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
	copyBufferSize int

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

// WithStatCache enables a cache of Stat results with the given TTL; misses
// are cached for half as long. See WithCacheConfig.
func WithStatCache(enabled bool, ttl time.Duration) Option {
	return func(ufs *UnionFS) {
		ufs.cache = newCache(enabled, ttl, ttl/2, 1000)
	}
}

// WithCacheConfig enables a cache of Stat results with separate TTLs for
// found and missing paths and a bound on entries per kind.
//
// Changes made through the union drop the cached entries at and under the
// paths they touch. Changes made to a layer directly, and changes seen
// through another path to the same file (a symbolic link), show only once
// the entry expires.
func WithCacheConfig(enabled bool, statTTL, negativeTTL time.Duration, maxEntries int) Option {
	return func(ufs *UnionFS) {
		ufs.cache = newCache(enabled, statTTL, negativeTTL, maxEntries)
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

// Name returns the name of the filesystem
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

// InvalidateCache removes a path from the cache
func (ufs *UnionFS) InvalidateCache(path string) {
	path = cleanPath(path)
	ufs.cache.invalidate(path)
}

// InvalidateCacheTree removes all cache entries under a path prefix
func (ufs *UnionFS) InvalidateCacheTree(pathPrefix string) {
	pathPrefix = cleanPath(pathPrefix)
	ufs.cache.invalidateTree(pathPrefix)
}

// ClearCache removes all cache entries
func (ufs *UnionFS) ClearCache() {
	ufs.cache.clear()
}

// CacheStats returns cache statistics
func (ufs *UnionFS) CacheStats() CacheStats {
	return ufs.cache.Stats()
}
