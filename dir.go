package unionfs

import (
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/spf13/afero"
)

// unionDir implements afero.File for directories, merging contents across layers
type unionDir struct {
	ufs        *UnionFS
	path       string
	entries    []os.FileInfo
	offset     int
	baseLayer  afero.Fs
	layerIdx   int
	closed     bool
}

// newUnionDir creates a new union directory
func newUnionDir(ufs *UnionFS, path string, baseLayer afero.Fs, layerIdx int) (*unionDir, error) {
	return &unionDir{
		ufs:       ufs,
		path:      path,
		baseLayer: baseLayer,
		layerIdx:  layerIdx,
		offset:    0,
		closed:    false,
	}, nil
}

// Close closes the directory
func (d *unionDir) Close() error {
	d.closed = true
	return nil
}

// Read is not supported for directories
func (d *unionDir) Read(p []byte) (n int, err error) {
	return 0, os.ErrInvalid
}

// ReadAt is not supported for directories
func (d *unionDir) ReadAt(p []byte, off int64) (n int, err error) {
	return 0, os.ErrInvalid
}

// Seek seeks to an offset in the directory listing
func (d *unionDir) Seek(offset int64, whence int) (int64, error) {
	if d.closed {
		return 0, os.ErrClosed
	}

	switch whence {
	case io.SeekStart:
		d.offset = int(offset)
	case io.SeekCurrent:
		d.offset += int(offset)
	case io.SeekEnd:
		if d.entries == nil {
			if err := d.loadEntries(); err != nil {
				return 0, err
			}
		}
		d.offset = len(d.entries) + int(offset)
	}

	if d.offset < 0 {
		d.offset = 0
	}

	return int64(d.offset), nil
}

// Write is not supported for directories
func (d *unionDir) Write(p []byte) (n int, err error) {
	return 0, os.ErrInvalid
}

// WriteAt is not supported for directories
func (d *unionDir) WriteAt(p []byte, off int64) (n int, err error) {
	return 0, os.ErrInvalid
}

// Name returns the base name of the directory
func (d *unionDir) Name() string {
	return path.Base(d.path)
}

// Readdir reads directory entries
func (d *unionDir) Readdir(count int) ([]os.FileInfo, error) {
	if d.closed {
		return nil, os.ErrClosed
	}

	if d.entries == nil {
		if err := d.loadEntries(); err != nil {
			return nil, err
		}
	}

	if d.offset >= len(d.entries) {
		if count > 0 {
			return nil, io.EOF
		}
		return nil, nil
	}

	var end int
	if count <= 0 {
		end = len(d.entries)
	} else {
		end = d.offset + count
		if end > len(d.entries) {
			end = len(d.entries)
		}
	}

	result := d.entries[d.offset:end]
	d.offset = end

	if count > 0 && len(result) == 0 {
		return nil, io.EOF
	}

	return result, nil
}

// Readdirnames reads directory entry names
func (d *unionDir) Readdirnames(count int) ([]string, error) {
	infos, err := d.Readdir(count)
	if err != nil {
		return nil, err
	}

	names := make([]string, len(infos))
	for i, info := range infos {
		names[i] = info.Name()
	}

	return names, nil
}

// Stat returns the FileInfo for the directory
func (d *unionDir) Stat() (os.FileInfo, error) {
	if d.closed {
		return nil, os.ErrClosed
	}
	return d.ufs.Stat(d.path)
}

// Sync is a no-op for directories
func (d *unionDir) Sync() error {
	return nil
}

// Truncate is not supported for directories
func (d *unionDir) Truncate(size int64) error {
	return os.ErrInvalid
}

// WriteString is not supported for directories
func (d *unionDir) WriteString(s string) (ret int, err error) {
	return 0, os.ErrInvalid
}

// loadEntries loads and merges directory entries from all layers
func (d *unionDir) loadEntries() error {
	seen := make(map[string]bool)
	whiteouts := make(map[string]bool)
	var entries []os.FileInfo

	d.ufs.mu.RLock()
	defer d.ufs.mu.RUnlock()

	// Check for opaque directory whiteout in upper layers
	isOpaque := false
	for i := 0; i < len(d.ufs.layers); i++ {
		layer := d.ufs.layers[i]
		opaquePath := path.Join(d.path, OpaqueWhiteout)
		if _, err := layer.fs.Stat(opaquePath); err == nil {
			isOpaque = true
			break
		}
	}

	// Iterate through all layers
	for i := 0; i < len(d.ufs.layers); i++ {
		// If we found an opaque whiteout, stop processing lower layers
		if isOpaque && i > 0 {
			break
		}

		layer := d.ufs.layers[i]

		// Try to read directory from this layer
		dir, err := layer.fs.Open(toAferoPath(d.path))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			// Skip layers with errors
			continue
		}

		// Read all entries from this layer
		layerEntries, err := dir.Readdir(-1)
		dir.Close()

		if err != nil {
			continue
		}

		// Process entries from this layer
		for _, entry := range layerEntries {
			name := entry.Name()

			// Check if this is a whiteout file
			if isWhiteout(name) {
				// Mark the original file as whited out
				if original, ok := originalPath(path.Join(d.path, name)); ok {
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

	d.entries = entries
	return nil
}
