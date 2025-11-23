// Package unionfs provides a layered filesystem implementation with Docker-style
// overlay capabilities and copy-on-write support.
package unionfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/afero"
)

const (
	// WhiteoutPrefix is the prefix for whiteout files (AUFS/Docker style)
	WhiteoutPrefix = ".wh."
	// OpaqueWhiteout marks a directory as opaque (hides all lower layer contents)
	OpaqueWhiteout = ".wh.__dir_opaque"
)

var (
	// ErrNoWritableLayer is returned when a write operation is attempted but no writable layer exists
	ErrNoWritableLayer = errors.New("no writable layer configured")
	// ErrReadOnlyLayer is returned when attempting to write to a read-only layer
	ErrReadOnlyLayer = errors.New("layer is read-only")
)

// Layer represents a single filesystem layer with metadata
type Layer struct {
	fs       afero.Fs
	readOnly bool
}

// UnionFS implements a union filesystem with multiple layers
type UnionFS struct {
	layers         []*Layer // ordered from top (highest precedence) to bottom
	writableLayer  *Layer   // reference to the writable layer (if any)
	mu             sync.RWMutex
	cache          *Cache
	copyBufferSize int
}

// Option is a functional option for configuring UnionFS
type Option func(*UnionFS)

// WithWritableLayer adds a writable layer at the top of the layer stack
func WithWritableLayer(fs afero.Fs) Option {
	return func(ufs *UnionFS) {
		layer := &Layer{fs: fs, readOnly: false}
		ufs.layers = append([]*Layer{layer}, ufs.layers...)
		ufs.writableLayer = layer
	}
}

// WithReadOnlyLayer adds a read-only layer to the layer stack
// Read-only layers are added in order after the writable layer
func WithReadOnlyLayer(fs afero.Fs) Option {
	return func(ufs *UnionFS) {
		layer := &Layer{fs: fs, readOnly: true}
		// Simply append - layers will be in order: writable, then read-only in order added
		ufs.layers = append(ufs.layers, layer)
	}
}

// WithStatCache enables stat caching with the specified TTL
func WithStatCache(enabled bool, ttl time.Duration) Option {
	return func(ufs *UnionFS) {
		negativeTTL := ttl / 2 // Negative cache expires faster
		maxEntries := 1000
		ufs.cache = newCache(enabled, ttl, negativeTTL, maxEntries)
	}
}

// WithCacheConfig enables caching with custom configuration
func WithCacheConfig(enabled bool, statTTL, negativeTTL time.Duration, maxEntries int) Option {
	return func(ufs *UnionFS) {
		ufs.cache = newCache(enabled, statTTL, negativeTTL, maxEntries)
	}
}

// WithCopyBufferSize sets the buffer size for copy-on-write operations
func WithCopyBufferSize(size int) Option {
	return func(ufs *UnionFS) {
		ufs.copyBufferSize = size
	}
}

// New creates a new UnionFS with the specified options
func New(opts ...Option) *UnionFS {
	ufs := &UnionFS{
		layers:         make([]*Layer, 0),
		copyBufferSize: 32 * 1024, // default 32KB
		cache:          newCache(false, 0, 0, 0), // disabled by default
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

// isWhiteout checks if a filename is a whiteout marker
func isWhiteout(name string) bool {
	base := filepath.Base(name)
	return strings.HasPrefix(base, WhiteoutPrefix)
}

// isOpaqueWhiteout checks if a directory contains an opaque whiteout marker
func isOpaqueWhiteout(name string) bool {
	return filepath.Base(name) == OpaqueWhiteout
}

// whiteoutPath returns the whiteout path for a given file path
func whiteoutPath(path string) string {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	return filepath.Join(dir, WhiteoutPrefix+base)
}

// originalPath returns the original path from a whiteout path
func originalPath(whiteoutPath string) (string, bool) {
	base := filepath.Base(whiteoutPath)
	if !strings.HasPrefix(base, WhiteoutPrefix) {
		return "", false
	}
	original := strings.TrimPrefix(base, WhiteoutPrefix)
	if original == "__dir_opaque" {
		return "", false
	}
	dir := filepath.Dir(whiteoutPath)
	return filepath.Join(dir, original), true
}

// cleanPath normalizes a path
func cleanPath(path string) string {
	cleaned := filepath.Clean(path)
	// Ensure paths are absolute or start with /
	if !filepath.IsAbs(cleaned) && !strings.HasPrefix(cleaned, "/") {
		cleaned = "/" + cleaned
	}
	return cleaned
}

// checkWhiteout checks if a file is marked as deleted via whiteout in any layer above the given index
func (ufs *UnionFS) checkWhiteout(path string, startLayer int) bool {
	wPath := whiteoutPath(path)
	for i := 0; i < startLayer; i++ {
		layer := ufs.layers[i]
		if _, err := layer.fs.Stat(wPath); err == nil {
			return true
		}
		// Check for opaque directory whiteout in parent directories
		dir := filepath.Dir(path)
		for dir != "/" && dir != "." {
			opaquePath := filepath.Join(dir, OpaqueWhiteout)
			if _, err := layer.fs.Stat(opaquePath); err == nil {
				return true
			}
			dir = filepath.Dir(dir)
		}
	}
	return false
}

// findFile searches for a file across all layers, respecting whiteouts
// Returns the file info, layer index, and error
func (ufs *UnionFS) findFile(path string) (os.FileInfo, int, error) {
	path = cleanPath(path)

	// Check cache first
	if info, layer, ok := ufs.cache.getStat(path); ok {
		return info, layer, nil
	}

	// Check negative cache
	if ufs.cache.isNegative(path) {
		return nil, -1, os.ErrNotExist
	}

	ufs.mu.RLock()
	defer ufs.mu.RUnlock()

	for i, layer := range ufs.layers {
		// Check if this file is whited out in an upper layer
		if ufs.checkWhiteout(path, i) {
			continue
		}

		info, err := layer.fs.Stat(path)
		if err == nil {
			// Found the file - cache it
			ufs.cache.putStat(path, info, i)
			return info, i, nil
		}
		if !os.IsNotExist(err) {
			// Real error (not just file not found)
			return nil, -1, err
		}
	}

	// File not found in any layer - cache negative result
	ufs.cache.putNegative(path)
	return nil, -1, os.ErrNotExist
}

// getWritableLayer returns the writable layer or an error
func (ufs *UnionFS) getWritableLayer() (*Layer, error) {
	ufs.mu.RLock()
	defer ufs.mu.RUnlock()

	if ufs.writableLayer == nil {
		return nil, ErrNoWritableLayer
	}
	return ufs.writableLayer, nil
}

// ensureDir ensures all parent directories exist in the writable layer
func (ufs *UnionFS) ensureDir(path string) error {
	layer, err := ufs.getWritableLayer()
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if dir == "/" || dir == "." {
		return nil
	}

	// Check if directory already exists
	if _, err := layer.fs.Stat(dir); err == nil {
		return nil
	}

	// Create directory with proper permissions
	return layer.fs.MkdirAll(dir, 0755)
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
