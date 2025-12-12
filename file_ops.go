package unionfs

import (
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/absfs/absfs"
)

// Stat returns file info, searching through layers
func (ufs *UnionFS) Stat(name string) (os.FileInfo, error) {
	info, _, err := ufs.findFile(name)
	return info, err
}

// Lstat returns file info without following symlinks
func (ufs *UnionFS) Lstat(name string) (os.FileInfo, error) {
	name = cleanPath(name)

	ufs.mu.RLock()
	defer ufs.mu.RUnlock()

	for i, layer := range ufs.layers {
		// Check if this file is whited out in an upper layer
		if ufs.checkWhiteout(name, i) {
			continue
		}

		// Try to lstat from this layer (using Lstat if available)
		if lstater, ok := layer.fs.(interface {
			Lstat(string) (os.FileInfo, error)
		}); ok {
			info, err := lstater.Lstat(name)
			if err == nil {
				return info, nil
			}
			if !os.IsNotExist(err) {
				return nil, err
			}
		} else {
			// Fallback to Stat if Lstat not available
			info, err := layer.fs.Stat(name)
			if err == nil {
				return info, nil
			}
			if !os.IsNotExist(err) {
				return nil, err
			}
		}
	}

	return nil, os.ErrNotExist
}

// Open opens a file for reading
func (ufs *UnionFS) Open(name string) (absfs.File, error) {
	return ufs.OpenFile(name, os.O_RDONLY, 0)
}

// OpenFile opens a file with the specified flags and permissions
func (ufs *UnionFS) OpenFile(name string, flag int, perm os.FileMode) (absfs.File, error) {
	name = cleanPath(name)

	// Check if this is a write operation
	isWrite := flag&(os.O_WRONLY|os.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_TRUNC) != 0

	if isWrite {
		// Write operations go to the writable layer
		layer, err := ufs.getWritableLayer()
		if err != nil {
			return nil, err
		}

		// Ensure parent directory exists
		if err := ufs.ensureDir(name); err != nil {
			return nil, err
		}

		// Check if file exists in a lower layer and needs copy-on-write
		if flag&os.O_CREATE == 0 || flag&os.O_EXCL == 0 {
			info, layerIdx, err := ufs.findFile(name)
			if err == nil && layerIdx > 0 {
				// File exists in a lower layer, copy it first
				if err := ufs.copyUp(name, info); err != nil {
					return nil, err
				}
			}
		}

		// Remove whiteout if it exists
		whiteout := whiteoutPath(name)
		layer.fs.Remove(whiteout)

		// Invalidate cache for this path since we're writing to it
		ufs.InvalidateCache(name)

		// Open file in writable layer
		return layer.fs.OpenFile(name, flag, perm)
	}

	// Read-only operation - find the file in layers
	info, layerIdx, err := ufs.findFile(name)
	if err != nil {
		return nil, err
	}

	layer := ufs.layers[layerIdx]
	if info.IsDir() {
		// For directories, we need to return a merged view
		return newUnionDir(ufs, name, layer.fs, layerIdx)
	}

	return layer.fs.Open(name)
}

// Create creates a file in the writable layer
func (ufs *UnionFS) Create(name string) (absfs.File, error) {
	return ufs.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
}

// Mkdir creates a directory in the writable layer
func (ufs *UnionFS) Mkdir(name string, perm os.FileMode) error {
	layer, err := ufs.getWritableLayer()
	if err != nil {
		return err
	}

	name = cleanPath(name)

	// Ensure parent directory exists
	if err := ufs.ensureDir(name); err != nil {
		return err
	}

	// Remove whiteout if it exists
	whiteout := whiteoutPath(name)
	layer.fs.Remove(whiteout)

	err = layer.fs.Mkdir(name, perm)
	if err == nil {
		ufs.InvalidateCache(name)
	}
	return err
}

// MkdirAll creates a directory and all parent directories
func (ufs *UnionFS) MkdirAll(name string, perm os.FileMode) error {
	layer, err := ufs.getWritableLayer()
	if err != nil {
		return err
	}

	name = cleanPath(name)

	// Remove whiteouts for this path and parents
	parts := splitPath(name)
	current := "/"
	for _, part := range parts {
		current = path.Join(current, part)
		whiteout := whiteoutPath(current)
		layer.fs.Remove(whiteout)
	}

	err = layer.fs.MkdirAll(name, perm)
	if err == nil {
		ufs.InvalidateCacheTree(name)
	}
	return err
}

// Remove deletes a file or empty directory by creating a whiteout
func (ufs *UnionFS) Remove(name string) error {
	layer, err := ufs.getWritableLayer()
	if err != nil {
		return err
	}

	name = cleanPath(name)

	// Check if file exists
	info, layerIdx, err := ufs.findFile(name)
	if err != nil {
		return err
	}

	// If file exists in writable layer, actually delete it
	if layerIdx == 0 {
		if err := layer.fs.Remove(name); err != nil {
			return err
		}
	}

	// If file exists in a lower layer, create whiteout
	if layerIdx > 0 || info != nil {
		whiteout := whiteoutPath(name)
		if err := ufs.ensureDir(whiteout); err != nil {
			return err
		}
		// Create empty whiteout file
		f, err := layer.fs.Create(whiteout)
		if err != nil {
			return err
		}
		f.Close()
	}

	ufs.InvalidateCache(name)
	return nil
}

// RemoveAll removes a path and all children
func (ufs *UnionFS) RemoveAll(name string) error {
	layer, err := ufs.getWritableLayer()
	if err != nil {
		return err
	}

	name = cleanPath(name)

	// Check if path exists
	info, layerIdx, err := ufs.findFile(name)
	if err != nil {
		return err
	}

	// If path exists in writable layer, remove it
	if layerIdx == 0 {
		if err := layer.fs.RemoveAll(name); err != nil {
			return err
		}
	}

	// If path exists in a lower layer, create whiteout to hide it
	if layerIdx > 0 {
		whiteout := whiteoutPath(name)
		if err := ufs.ensureDir(whiteout); err != nil {
			return err
		}
		// Create empty whiteout file
		f, err := layer.fs.Create(whiteout)
		if err != nil {
			return err
		}
		f.Close()
	}

	// Suppress unused variable warning
	_ = info

	ufs.InvalidateCacheTree(name)
	return nil
}

// Rename renames a file or directory
func (ufs *UnionFS) Rename(oldname, newname string) error {
	layer, err := ufs.getWritableLayer()
	if err != nil {
		return err
	}

	oldname = cleanPath(oldname)
	newname = cleanPath(newname)

	// Check if old file exists
	info, layerIdx, err := ufs.findFile(oldname)
	if err != nil {
		return err
	}

	// If file is in a lower layer, copy it up first
	if layerIdx > 0 {
		if err := ufs.copyUp(oldname, info); err != nil {
			return err
		}
	}

	// Ensure destination directory exists
	if err := ufs.ensureDir(newname); err != nil {
		return err
	}

	// Remove whiteout for new name if it exists
	newWhiteout := whiteoutPath(newname)
	layer.fs.Remove(newWhiteout)

	// Perform rename in writable layer
	if err := layer.fs.Rename(oldname, newname); err != nil {
		return err
	}

	// Create whiteout for old name if it existed in a lower layer
	if layerIdx > 0 {
		oldWhiteout := whiteoutPath(oldname)
		if err := ufs.ensureDir(oldWhiteout); err != nil {
			return err
		}
		f, err := layer.fs.Create(oldWhiteout)
		if err != nil {
			return err
		}
		f.Close()
	}

	ufs.InvalidateCache(oldname)
	ufs.InvalidateCache(newname)
	return nil
}

// Chmod changes file permissions
func (ufs *UnionFS) Chmod(name string, mode os.FileMode) error {
	layer, err := ufs.getWritableLayer()
	if err != nil {
		return err
	}

	name = cleanPath(name)

	// Check if file exists and copy up if needed
	info, layerIdx, err := ufs.findFile(name)
	if err != nil {
		return err
	}

	if layerIdx > 0 {
		if err := ufs.copyUp(name, info); err != nil {
			return err
		}
	}

	err = layer.fs.Chmod(name, mode)
	if err == nil {
		ufs.InvalidateCache(name)
	}
	return err
}

// Chown changes file ownership
func (ufs *UnionFS) Chown(name string, uid, gid int) error {
	layer, err := ufs.getWritableLayer()
	if err != nil {
		return err
	}

	name = cleanPath(name)

	// Check if file exists and copy up if needed
	info, layerIdx, err := ufs.findFile(name)
	if err != nil {
		return err
	}

	if layerIdx > 0 {
		if err := ufs.copyUp(name, info); err != nil {
			return err
		}
	}

	err = layer.fs.Chown(name, uid, gid)
	if err == nil {
		ufs.InvalidateCache(name)
	}
	return err
}

// Chtimes changes file access and modification times
func (ufs *UnionFS) Chtimes(name string, atime, mtime time.Time) error {
	layer, err := ufs.getWritableLayer()
	if err != nil {
		return err
	}

	name = cleanPath(name)

	// Check if file exists and copy up if needed
	info, layerIdx, err := ufs.findFile(name)
	if err != nil {
		return err
	}

	if layerIdx > 0 {
		if err := ufs.copyUp(name, info); err != nil {
			return err
		}
	}

	err = layer.fs.Chtimes(name, atime, mtime)
	if err == nil {
		ufs.InvalidateCache(name)
	}
	return err
}

// splitPath splits a path into components
func splitPath(p string) []string {
	p = cleanPath(p)
	if p == "/" {
		return []string{}
	}
	// Use path.Clean for virtual paths
	p = path.Clean(p)
	return splitPathHelper(p)
}

func splitPathHelper(p string) []string {
	if p == "/" || p == "." {
		return []string{}
	}
	dir, file := path.Split(p)
	if dir == "" || dir == "/" {
		return []string{file}
	}
	return append(splitPathHelper(path.Clean(dir)), file)
}

// ReadDir reads the named directory and returns all its directory entries,
// merging results from all layers. Entries from upper layers take precedence,
// and whiteouts are respected.
func (ufs *UnionFS) ReadDir(name string) ([]fs.DirEntry, error) {
	name = cleanPath(name)

	// Check if the directory exists in any layer
	info, _, err := ufs.findFile(name)
	if err != nil {
		return nil, err
	}

	if !info.IsDir() {
		return nil, &os.PathError{Op: "readdir", Path: name, Err: os.ErrInvalid}
	}

	seen := make(map[string]bool)
	whiteouts := make(map[string]bool)
	var entries []fs.DirEntry

	ufs.mu.RLock()
	defer ufs.mu.RUnlock()

	// Check for opaque directory whiteout in upper layers
	isOpaque := false
	for i := 0; i < len(ufs.layers); i++ {
		layer := ufs.layers[i]
		opaquePath := path.Join(name, OpaqueWhiteout)
		if _, err := layer.fs.Stat(opaquePath); err == nil {
			isOpaque = true
			break
		}
	}

	// Iterate through all layers
	for i := 0; i < len(ufs.layers); i++ {
		// If we found an opaque whiteout, stop processing lower layers
		if isOpaque && i > 0 {
			break
		}

		layer := ufs.layers[i]

		// Try to read directory from this layer using ReadDir if available
		var layerEntries []fs.DirEntry
		if reader, ok := layer.fs.(interface{ ReadDir(string) ([]fs.DirEntry, error) }); ok {
			layerEntries, err = reader.ReadDir(name)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				continue
			}
		} else {
			// Fallback to Open + Readdir
			dir, err := layer.fs.Open(name)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				continue
			}

			infos, err := dir.Readdir(-1)
			dir.Close()

			if err != nil {
				continue
			}

			// Convert FileInfo to DirEntry
			layerEntries = make([]fs.DirEntry, len(infos))
			for j, info := range infos {
				layerEntries[j] = fs.FileInfoToDirEntry(info)
			}
		}

		// Process entries from this layer
		for _, entry := range layerEntries {
			name := entry.Name()

			// Check if this is a whiteout file
			if isWhiteout(name) {
				// Mark the original file as whited out
				if original, ok := originalPath(path.Join(name, name)); ok {
					whiteouts[path.Base(original)] = true
				}
				continue
			}

			// Skip opaque whiteout markers
			if isOpaqueWhiteout(name) {
				continue
			}

			// Skip if already seen in upper layer
			if seen[name] {
				continue
			}

			// Skip if whited out
			if whiteouts[name] {
				continue
			}

			// Add entry
			seen[name] = true
			entries = append(entries, entry)
		}
	}

	// Sort entries by name
	sort.Slice(entries, func(i, j int) bool {
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})

	return entries, nil
}

// ReadFile reads the named file and returns its contents.
// It reads from the first layer (highest precedence) that contains the file.
func (ufs *UnionFS) ReadFile(name string) ([]byte, error) {
	name = cleanPath(name)

	// Find the file in the layers
	info, layerIdx, err := ufs.findFile(name)
	if err != nil {
		return nil, err
	}

	if info.IsDir() {
		return nil, &os.PathError{Op: "read", Path: name, Err: os.ErrInvalid}
	}

	ufs.mu.RLock()
	layer := ufs.layers[layerIdx]
	ufs.mu.RUnlock()

	// Try to use ReadFile if available
	if reader, ok := layer.fs.(interface{ ReadFile(string) ([]byte, error) }); ok {
		return reader.ReadFile(name)
	}

	// Fallback to Open + Read
	file, err := layer.fs.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	return io.ReadAll(file)
}

// Sub returns a filesystem rooted at the given directory, creating a
// sub-view across all layers.
func (ufs *UnionFS) Sub(dir string) (fs.FS, error) {
	dir = cleanPath(dir)
	adapter := &absFSAdapter{ufs: ufs}
	return absfs.FilerToFS(adapter, dir)
}
