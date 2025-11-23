package unionfs

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/afero"
)

// TestBasicReadThrough tests reading files from lower layers
func TestBasicReadThrough(t *testing.T) {
	// Create base layer with a file
	baseLayer := afero.NewMemMapFs()
	afero.WriteFile(baseLayer, "/test.txt", []byte("base content"), 0644)

	// Create writable overlay
	overlay := afero.NewMemMapFs()

	// Create union filesystem
	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(baseLayer),
	)

	// Read file from base layer
	data, err := afero.ReadFile(ufs, "/test.txt")
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}

	if string(data) != "base content" {
		t.Errorf("expected 'base content', got '%s'", string(data))
	}
}

// TestWriteToOverlay tests writing files to the overlay layer
func TestWriteToOverlay(t *testing.T) {
	baseLayer := afero.NewMemMapFs()
	overlay := afero.NewMemMapFs()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(baseLayer),
	)

	// Write new file
	err := afero.WriteFile(ufs, "/new.txt", []byte("new content"), 0644)
	if err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	// Read it back
	data, err := afero.ReadFile(ufs, "/new.txt")
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}

	if string(data) != "new content" {
		t.Errorf("expected 'new content', got '%s'", string(data))
	}

	// Verify file exists in overlay, not in base
	if _, err := overlay.Stat("/new.txt"); err != nil {
		t.Error("file should exist in overlay layer")
	}

	if _, err := baseLayer.Stat("/new.txt"); err == nil {
		t.Error("file should not exist in base layer")
	}
}

// TestCopyOnWrite tests that modifying a file in a lower layer triggers copy-on-write
func TestCopyOnWrite(t *testing.T) {
	// Create base layer with a file
	baseLayer := afero.NewMemMapFs()
	afero.WriteFile(baseLayer, "/test.txt", []byte("original"), 0644)

	overlay := afero.NewMemMapFs()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(baseLayer),
	)

	// Open file for writing
	f, err := ufs.OpenFile("/test.txt", os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatalf("failed to open file: %v", err)
	}

	// Write new content
	_, err = f.Write([]byte("modified"))
	if err != nil {
		t.Fatalf("failed to write: %v", err)
	}
	f.Close()

	// Read from union - should get modified content
	data, err := afero.ReadFile(ufs, "/test.txt")
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}

	if string(data) != "modified" {
		t.Errorf("expected 'modified', got '%s'", string(data))
	}

	// Base layer should still have original content
	data, err = afero.ReadFile(baseLayer, "/test.txt")
	if err != nil {
		t.Fatalf("failed to read from base: %v", err)
	}

	if string(data) != "original" {
		t.Errorf("base layer should still have 'original', got '%s'", string(data))
	}
}

// TestWhiteout tests file deletion using whiteout markers
func TestWhiteout(t *testing.T) {
	// Create base layer with files
	baseLayer := afero.NewMemMapFs()
	afero.WriteFile(baseLayer, "/file1.txt", []byte("content1"), 0644)
	afero.WriteFile(baseLayer, "/file2.txt", []byte("content2"), 0644)

	overlay := afero.NewMemMapFs()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(baseLayer),
	)

	// Delete file1
	err := ufs.Remove("/file1.txt")
	if err != nil {
		t.Fatalf("failed to remove file: %v", err)
	}

	// file1 should not be visible
	if _, err := ufs.Stat("/file1.txt"); err == nil {
		t.Error("file1 should not exist after removal")
	}

	// Whiteout marker should exist
	whiteout := whiteoutPath("/file1.txt")
	if _, err := overlay.Stat(whiteout); err != nil {
		t.Errorf("whiteout marker should exist at %s", whiteout)
	}

	// file2 should still be visible
	if _, err := ufs.Stat("/file2.txt"); err != nil {
		t.Error("file2 should still exist")
	}
}

// TestLayerPrecedence tests that upper layers take precedence
func TestLayerPrecedence(t *testing.T) {
	layer0 := afero.NewMemMapFs()
	layer1 := afero.NewMemMapFs()
	layer2 := afero.NewMemMapFs()

	// Write same file to all layers with different content
	afero.WriteFile(layer0, "/test.txt", []byte("layer0"), 0644)
	afero.WriteFile(layer1, "/test.txt", []byte("layer1"), 0644)

	ufs := New(
		WithWritableLayer(layer2),
		WithReadOnlyLayer(layer1),
		WithReadOnlyLayer(layer0),
	)

	// Should read from layer1 (highest non-empty layer)
	data, err := afero.ReadFile(ufs, "/test.txt")
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}

	if string(data) != "layer1" {
		t.Errorf("expected 'layer1', got '%s'", string(data))
	}

	// Write to overlay
	afero.WriteFile(ufs, "/test.txt", []byte("layer2"), 0644)

	// Should now read from layer2
	data, err = afero.ReadFile(ufs, "/test.txt")
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}

	if string(data) != "layer2" {
		t.Errorf("expected 'layer2', got '%s'", string(data))
	}
}

// TestDirectoryMerging tests merging directory contents across layers
func TestDirectoryMerging(t *testing.T) {
	layer0 := afero.NewMemMapFs()
	layer1 := afero.NewMemMapFs()
	overlay := afero.NewMemMapFs()

	// Create files in different layers
	afero.WriteFile(layer0, "/dir/file1.txt", []byte("1"), 0644)
	afero.WriteFile(layer0, "/dir/file2.txt", []byte("2"), 0644)
	afero.WriteFile(layer1, "/dir/file3.txt", []byte("3"), 0644)
	afero.WriteFile(overlay, "/dir/file4.txt", []byte("4"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(layer1),
		WithReadOnlyLayer(layer0),
	)

	// Read directory
	entries, err := afero.ReadDir(ufs, "/dir")
	if err != nil {
		t.Fatalf("failed to read directory: %v", err)
	}

	// Should have 4 files
	if len(entries) != 4 {
		t.Errorf("expected 4 entries, got %d", len(entries))
	}

	// Check all files are present
	names := make(map[string]bool)
	for _, entry := range entries {
		names[entry.Name()] = true
	}

	expected := []string{"file1.txt", "file2.txt", "file3.txt", "file4.txt"}
	for _, name := range expected {
		if !names[name] {
			t.Errorf("expected file %s not found", name)
		}
	}
}

// TestDirectoryMergingWithWhiteout tests that whiteouts hide files in merged directories
func TestDirectoryMergingWithWhiteout(t *testing.T) {
	baseLayer := afero.NewMemMapFs()
	overlay := afero.NewMemMapFs()

	// Create files in base layer
	afero.WriteFile(baseLayer, "/dir/file1.txt", []byte("1"), 0644)
	afero.WriteFile(baseLayer, "/dir/file2.txt", []byte("2"), 0644)
	afero.WriteFile(baseLayer, "/dir/file3.txt", []byte("3"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(baseLayer),
	)

	// Delete file2
	ufs.Remove("/dir/file2.txt")

	// Read directory
	entries, err := afero.ReadDir(ufs, "/dir")
	if err != nil {
		t.Fatalf("failed to read directory: %v", err)
	}

	// Should have 2 files (file1 and file3, not file2)
	if len(entries) != 2 {
		t.Errorf("expected 2 entries, got %d", len(entries))
	}

	names := make(map[string]bool)
	for _, entry := range entries {
		names[entry.Name()] = true
	}

	if names["file2.txt"] {
		t.Error("file2.txt should not appear in directory listing")
	}

	if !names["file1.txt"] || !names["file3.txt"] {
		t.Error("file1.txt and file3.txt should appear in directory listing")
	}
}

// TestMkdir tests creating directories
func TestMkdir(t *testing.T) {
	baseLayer := afero.NewMemMapFs()
	overlay := afero.NewMemMapFs()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(baseLayer),
	)

	// Create directory
	err := ufs.Mkdir("/newdir", 0755)
	if err != nil {
		t.Fatalf("failed to create directory: %v", err)
	}

	// Verify it exists
	info, err := ufs.Stat("/newdir")
	if err != nil {
		t.Fatalf("directory should exist: %v", err)
	}

	if !info.IsDir() {
		t.Error("should be a directory")
	}

	// Verify it's in overlay
	if _, err := overlay.Stat("/newdir"); err != nil {
		t.Error("directory should exist in overlay")
	}
}

// TestMkdirAll tests creating nested directories
func TestMkdirAll(t *testing.T) {
	baseLayer := afero.NewMemMapFs()
	overlay := afero.NewMemMapFs()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(baseLayer),
	)

	// Create nested directories
	err := ufs.MkdirAll("/a/b/c", 0755)
	if err != nil {
		t.Fatalf("failed to create directories: %v", err)
	}

	// Verify all exist
	for _, path := range []string{"/a", "/a/b", "/a/b/c"} {
		info, err := ufs.Stat(path)
		if err != nil {
			t.Errorf("directory %s should exist: %v", path, err)
		}
		if info != nil && !info.IsDir() {
			t.Errorf("%s should be a directory", path)
		}
	}
}

// TestRename tests renaming files
func TestRename(t *testing.T) {
	baseLayer := afero.NewMemMapFs()
	overlay := afero.NewMemMapFs()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(baseLayer),
	)

	// Create file
	afero.WriteFile(ufs, "/old.txt", []byte("content"), 0644)

	// Rename it
	err := ufs.Rename("/old.txt", "/new.txt")
	if err != nil {
		t.Fatalf("failed to rename: %v", err)
	}

	// Old should not exist
	if _, err := ufs.Stat("/old.txt"); err == nil {
		t.Error("old file should not exist")
	}

	// New should exist with same content
	data, err := afero.ReadFile(ufs, "/new.txt")
	if err != nil {
		t.Fatalf("failed to read renamed file: %v", err)
	}

	if string(data) != "content" {
		t.Errorf("expected 'content', got '%s'", string(data))
	}
}

// TestRenameCopyUp tests renaming a file from a lower layer
func TestRenameCopyUp(t *testing.T) {
	baseLayer := afero.NewMemMapFs()
	overlay := afero.NewMemMapFs()

	// Create file in base layer
	afero.WriteFile(baseLayer, "/base.txt", []byte("base content"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(baseLayer),
	)

	// Rename file from base layer
	err := ufs.Rename("/base.txt", "/renamed.txt")
	if err != nil {
		t.Fatalf("failed to rename: %v", err)
	}

	// Original should not be visible (whiteout)
	if _, err := ufs.Stat("/base.txt"); err == nil {
		t.Error("original file should not be visible")
	}

	// Renamed should exist in overlay
	data, err := afero.ReadFile(ufs, "/renamed.txt")
	if err != nil {
		t.Fatalf("failed to read renamed file: %v", err)
	}

	if string(data) != "base content" {
		t.Errorf("expected 'base content', got '%s'", string(data))
	}

	// Base layer should still have original
	if _, err := baseLayer.Stat("/base.txt"); err != nil {
		t.Error("base layer should still have original file")
	}
}

// TestRemoveAll tests removing directories recursively
func TestRemoveAll(t *testing.T) {
	baseLayer := afero.NewMemMapFs()
	overlay := afero.NewMemMapFs()

	// Create directory structure in base
	baseLayer.MkdirAll("/dir/subdir", 0755)
	afero.WriteFile(baseLayer, "/dir/file1.txt", []byte("1"), 0644)
	afero.WriteFile(baseLayer, "/dir/subdir/file2.txt", []byte("2"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(baseLayer),
	)

	// Remove directory
	err := ufs.RemoveAll("/dir")
	if err != nil {
		t.Fatalf("failed to remove directory: %v", err)
	}

	// Directory should not be visible
	if _, err := ufs.Stat("/dir"); err == nil {
		t.Error("directory should not exist after RemoveAll")
	}

	// Whiteout should exist
	whiteout := whiteoutPath("/dir")
	if _, err := overlay.Stat(whiteout); err != nil {
		t.Error("whiteout should exist for removed directory")
	}
}

// TestChmod tests changing file permissions
func TestChmod(t *testing.T) {
	baseLayer := afero.NewMemMapFs()
	overlay := afero.NewMemMapFs()

	// Create file in base layer
	afero.WriteFile(baseLayer, "/test.txt", []byte("content"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(baseLayer),
	)

	// Change permissions
	err := ufs.Chmod("/test.txt", 0600)
	if err != nil {
		t.Fatalf("failed to chmod: %v", err)
	}

	// File should now exist in overlay with new permissions
	info, err := overlay.Stat("/test.txt")
	if err != nil {
		t.Fatalf("file should exist in overlay: %v", err)
	}

	if info.Mode().Perm() != 0600 {
		t.Errorf("expected permissions 0600, got %o", info.Mode().Perm())
	}
}

// TestNoWritableLayer tests that operations fail when no writable layer is configured
func TestNoWritableLayer(t *testing.T) {
	baseLayer := afero.NewMemMapFs()
	afero.WriteFile(baseLayer, "/test.txt", []byte("content"), 0644)

	// Create union with only read-only layers
	ufs := New(
		WithReadOnlyLayer(baseLayer),
	)

	// Try to write - should fail
	err := afero.WriteFile(ufs, "/new.txt", []byte("new"), 0644)
	if err != ErrNoWritableLayer {
		t.Errorf("expected ErrNoWritableLayer, got %v", err)
	}

	// Try to remove - should fail
	err = ufs.Remove("/test.txt")
	if err != ErrNoWritableLayer {
		t.Errorf("expected ErrNoWritableLayer, got %v", err)
	}
}

// TestOpenForAppend tests opening a file for append
func TestOpenForAppend(t *testing.T) {
	baseLayer := afero.NewMemMapFs()
	overlay := afero.NewMemMapFs()

	afero.WriteFile(baseLayer, "/test.txt", []byte("original\n"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(baseLayer),
	)

	// Open for append
	f, err := ufs.OpenFile("/test.txt", os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("failed to open for append: %v", err)
	}

	// Append content
	_, err = f.Write([]byte("appended\n"))
	if err != nil {
		t.Fatalf("failed to append: %v", err)
	}
	f.Close()

	// Read back
	data, err := afero.ReadFile(ufs, "/test.txt")
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}

	expected := "original\nappended\n"
	if string(data) != expected {
		t.Errorf("expected '%s', got '%s'", expected, string(data))
	}
}

// TestSeekInDirectory tests seeking within directory listings
func TestSeekInDirectory(t *testing.T) {
	baseLayer := afero.NewMemMapFs()
	overlay := afero.NewMemMapFs()

	// Create multiple files
	for i := 1; i <= 5; i++ {
		filename := filepath.Join("/dir", "file"+string(rune('0'+i))+".txt")
		afero.WriteFile(baseLayer, filename, []byte("content"), 0644)
	}

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(baseLayer),
	)

	// Open directory
	dir, err := ufs.Open("/dir")
	if err != nil {
		t.Fatalf("failed to open directory: %v", err)
	}
	defer dir.Close()

	// Read first 2 entries
	entries, err := dir.Readdir(2)
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 entries, got %d", len(entries))
	}

	// Seek to beginning
	_, err = dir.Seek(0, io.SeekStart)
	if err != nil {
		t.Fatalf("failed to seek: %v", err)
	}

	// Read all entries
	entries, err = dir.Readdir(-1)
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}
	if len(entries) != 5 {
		t.Errorf("expected 5 entries after seek, got %d", len(entries))
	}
}
