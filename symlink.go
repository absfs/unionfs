package unionfs

import (
	"os"
	"path"
	"strings"
)

// Readlink returns the destination of a symlink
func (ufs *UnionFS) Readlink(name string) (string, error) {
	name = cleanPath(name)

	ufs.mu.RLock()
	defer ufs.mu.RUnlock()

	// Search for symlink across layers
	for i, layer := range ufs.layers {
		// Check if this file is whited out in an upper layer
		if ufs.checkWhiteout(name, i) {
			continue
		}

		// Try to read symlink from this layer
		if linker, ok := layer.fs.(interface {
			Readlink(string) (string, error)
		}); ok {
			target, err := linker.Readlink(name)
			if err == nil {
				return target, nil
			}
			if !os.IsNotExist(err) {
				// Real error (not just file not found)
				return "", err
			}
		}
	}

	return "", os.ErrNotExist
}

// Symlink creates a symbolic link
func (ufs *UnionFS) Symlink(oldname, newname string) error {
	layer, err := ufs.getWritableLayer()
	if err != nil {
		return err
	}

	newname = cleanPath(newname)

	// Ensure parent directory exists
	if err := ufs.ensureDir(newname); err != nil {
		return err
	}

	// Remove whiteout if it exists
	whiteout := whiteoutPath(newname)
	layer.fs.Remove(whiteout)

	// Create symlink using the underlying filesystem's capability
	if linker, ok := layer.fs.(interface {
		Symlink(string, string) error
	}); ok {
		return linker.Symlink(oldname, newname)
	}

	// If the underlying filesystem doesn't support symlinks, return error
	return os.ErrInvalid
}

// Lchown changes the ownership of a symlink (without following it)
func (ufs *UnionFS) Lchown(name string, uid, gid int) error {
	layer, err := ufs.getWritableLayer()
	if err != nil {
		return err
	}

	name = cleanPath(name)

	// Get file info without following symlinks
	info, err := ufs.Lstat(name)
	if err != nil {
		return err
	}

	// Find which layer has the file
	ufs.mu.RLock()
	layerIdx := -1
	for i, l := range ufs.layers {
		if ufs.checkWhiteout(name, i) {
			continue
		}
		_, err := l.fs.Stat(name)
		if err == nil {
			layerIdx = i
			break
		}
	}
	ufs.mu.RUnlock()

	// Copy up if file is in a lower layer
	if layerIdx > 0 {
		if err := ufs.copyUp(name, info); err != nil {
			return err
		}
	}

	// Try Lchown if supported, otherwise fall back to Chown
	if lchowner, ok := layer.fs.(interface {
		Lchown(string, int, int) error
	}); ok {
		err = lchowner.Lchown(name, uid, gid)
	} else {
		// Fallback to regular Chown - may follow symlinks on some systems
		err = layer.fs.Chown(name, uid, gid)
	}

	if err == nil {
		ufs.InvalidateCache(name)
	}
	return err
}

// LstatIfPossible returns file info without following symlinks if the filesystem supports it
func (ufs *UnionFS) LstatIfPossible(name string) (os.FileInfo, bool, error) {
	name = cleanPath(name)

	ufs.mu.RLock()
	defer ufs.mu.RUnlock()

	for i, layer := range ufs.layers {
		// Check if this file is whited out in an upper layer
		if ufs.checkWhiteout(name, i) {
			continue
		}

		// Try to lstat from this layer
		if lstater, ok := layer.fs.(interface {
			LstatIfPossible(string) (os.FileInfo, bool, error)
		}); ok {
			info, supported, err := lstater.LstatIfPossible(name)
			if err == nil {
				return info, supported, nil
			}
			if !os.IsNotExist(err) {
				return nil, supported, err
			}
		} else {
			// Fall back to regular Stat if Lstat not supported
			info, err := layer.fs.Stat(name)
			if err == nil {
				return info, false, nil
			}
			if !os.IsNotExist(err) {
				return nil, false, err
			}
		}
	}

	return nil, false, os.ErrNotExist
}

// ReadlinkIfPossible returns the destination of a symlink if supported
func (ufs *UnionFS) ReadlinkIfPossible(name string) (string, error) {
	return ufs.Readlink(name)
}

// SymlinkIfPossible creates a symbolic link if supported
func (ufs *UnionFS) SymlinkIfPossible(oldname, newname string) error {
	return ufs.Symlink(oldname, newname)
}

// resolveSymlink resolves a symlink path within the union filesystem
// This is used internally to follow symlinks during path resolution
func (ufs *UnionFS) resolveSymlink(p string, maxDepth int) (string, error) {
	if maxDepth <= 0 {
		return "", os.ErrInvalid // Too many symbolic links
	}

	info, supported, err := ufs.LstatIfPossible(p)
	if err != nil {
		return "", err
	}

	// If not a symlink or Lstat not supported, return as-is
	if !supported || info.Mode()&os.ModeSymlink == 0 {
		return p, nil
	}

	// Read the symlink target
	target, err := ufs.Readlink(p)
	if err != nil {
		return "", err
	}

	// Handle absolute vs relative symlinks
	var resolved string
	if path.IsAbs(target) {
		resolved = target
	} else {
		// Relative symlink - resolve relative to parent directory
		dir := path.Dir(p)
		resolved = path.Join(dir, target)
	}

	resolved = cleanPath(resolved)

	// Recursively resolve if the target is also a symlink
	return ufs.resolveSymlink(resolved, maxDepth-1)
}

// followSymlinks follows symlinks in a path up to a maximum depth
func (ufs *UnionFS) followSymlinks(path string) (string, error) {
	const maxSymlinkDepth = 40 // Same as Linux MAXSYMLINKS
	return ufs.resolveSymlink(path, maxSymlinkDepth)
}

// isSymlinkLoop detects if following a symlink would create a loop
func isSymlinkLoop(p, target string, visited map[string]bool) bool {
	// Clean and normalize both paths
	p = cleanPath(p)

	var resolvedTarget string
	if path.IsAbs(target) {
		resolvedTarget = cleanPath(target)
	} else {
		dir := path.Dir(p)
		resolvedTarget = cleanPath(path.Join(dir, target))
	}

	// Check if we've already visited this path
	if visited[resolvedTarget] {
		return true
	}

	// Check if target points back to path or any parent
	parts := strings.Split(strings.Trim(resolvedTarget, "/"), "/")
	for i := range parts {
		check := "/" + strings.Join(parts[:i+1], "/")
		if visited[check] {
			return true
		}
	}

	return false
}
