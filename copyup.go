package unionfs

import (
	"fmt"
	"io"
	"os"
	"path"
)

// copyUp copies a file from a lower layer to the writable layer
func (ufs *UnionFS) copyUp(path string, info os.FileInfo) error {
	layer, err := ufs.getWritableLayer()
	if err != nil {
		return err
	}

	// Check if file already exists in writable layer
	if _, err := layer.fs.Stat(path); err == nil {
		// File already exists in writable layer, nothing to do
		return nil
	}

	// Ensure parent directory exists
	if err := ufs.ensureDir(path); err != nil {
		return err
	}

	// Handle directories
	if info.IsDir() {
		return ufs.copyUpDir(path, info)
	}

	// Handle regular files
	return ufs.copyUpFile(path, info)
}

// copyUpFile copies a regular file to the writable layer
func (ufs *UnionFS) copyUpFile(path string, info os.FileInfo) error {
	layer, err := ufs.getWritableLayer()
	if err != nil {
		return err
	}

	// Find the source file in lower layers
	_, layerIdx, err := ufs.findFile(path)
	if err != nil {
		return err
	}

	if layerIdx == 0 {
		// Already in writable layer
		return nil
	}

	sourceLayer := ufs.layers[layerIdx]

	// Open source file
	srcFile, err := sourceLayer.fs.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open source file: %w", err)
	}
	defer srcFile.Close()

	// Create destination file
	dstFile, err := layer.fs.OpenFile(toAferoPath(path), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return fmt.Errorf("failed to create destination file: %w", err)
	}
	defer dstFile.Close()

	// Copy file contents
	buf := make([]byte, ufs.copyBufferSize)
	if _, err := io.CopyBuffer(dstFile, srcFile, buf); err != nil {
		return fmt.Errorf("failed to copy file contents: %w", err)
	}

	// Preserve file metadata
	if err := layer.fs.Chmod(toAferoPath(path), info.Mode()); err != nil {
		return fmt.Errorf("failed to set file mode: %w", err)
	}

	if err := layer.fs.Chtimes(toAferoPath(path), info.ModTime(), info.ModTime()); err != nil {
		// Non-fatal error
		_ = err
	}

	return nil
}

// copyUpDir creates a directory in the writable layer
func (ufs *UnionFS) copyUpDir(path string, info os.FileInfo) error {
	layer, err := ufs.getWritableLayer()
	if err != nil {
		return err
	}

	// Create directory in writable layer
	if err := layer.fs.MkdirAll(toAferoPath(path), info.Mode()); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Preserve directory metadata
	if err := layer.fs.Chmod(toAferoPath(path), info.Mode()); err != nil {
		return fmt.Errorf("failed to set directory mode: %w", err)
	}

	if err := layer.fs.Chtimes(toAferoPath(path), info.ModTime(), info.ModTime()); err != nil {
		// Non-fatal error
		_ = err
	}

	return nil
}

// copyUpParents ensures all parent directories exist in the writable layer
func (ufs *UnionFS) copyUpParents(p string) error {
	dir := path.Dir(p)
	if dir == "/" || dir == "." {
		return nil
	}

	// Check if parent directory exists in any layer
	info, layerIdx, err := ufs.findFile(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// Parent doesn't exist, create it
			layer, err := ufs.getWritableLayer()
			if err != nil {
				return err
			}
			return layer.fs.MkdirAll(toAferoPath(dir), 0755)
		}
		return err
	}

	// If parent exists in a lower layer, copy it up
	if layerIdx > 0 && info.IsDir() {
		return ufs.copyUpDir(dir, info)
	}

	return nil
}
