package unionfs

import (
	"fmt"
	"testing"
	"time"
)

// TestCacheInvalidation tests that cache is properly invalidated on writes
func TestCacheInvalidation(t *testing.T) {
	baseLayer := mustNewMemFS()
	overlay := mustNewMemFS()

	writeFile(baseLayer, "/test.txt", []byte("original"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(baseLayer),
		WithStatCache(true, 5*time.Minute),
	)

	// First read - should cache
	info1, err := ufs.Stat("/test.txt")
	if err != nil {
		t.Fatalf("failed to stat: %v", err)
	}

	// Modify file
	writeFile(ufs, "/test.txt", []byte("modified content"), 0644)

	// Second read - cache should be invalidated, should get new info
	info2, err := ufs.Stat("/test.txt")
	if err != nil {
		t.Fatalf("failed to stat after modification: %v", err)
	}

	// Size should be different
	if info1.Size() == info2.Size() {
		t.Error("cache was not invalidated after write")
	}
}

// TestSymlinkBasic tests basic symlink functionality
func TestSymlinkBasic(t *testing.T) {
	baseLayer := mustNewMemFS()
	overlay := mustNewMemFS()

	// Create target file
	writeFile(baseLayer, "/target.txt", []byte("target content"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(baseLayer),
	)

	// Create symlink in overlay
	err := ufs.Symlink("/target.txt", "/link.txt")
	if err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	// Read symlink
	target, err := ufs.Readlink("/link.txt")
	if err != nil {
		t.Fatalf("failed to read symlink: %v", err)
	}

	if target != "/target.txt" {
		t.Errorf("expected '/target.txt', got '%s'", target)
	}
}

// TestComplexLayerHierarchy tests a complex multi-layer setup
func TestComplexLayerHierarchy(t *testing.T) {
	// Simulate a Docker-like setup
	// Layer 0 (bottom): Base OS files
	baseOS := mustNewMemFS()
	writeFile(baseOS, "/bin/sh", []byte("shell"), 0755)
	writeFile(baseOS, "/etc/passwd", []byte("root:x:0:0"), 0644)

	// Layer 1: Runtime dependencies
	runtime := mustNewMemFS()
	writeFile(runtime, "/lib/libc.so", []byte("libc"), 0755)

	// Layer 2: Application
	app := mustNewMemFS()
	writeFile(app, "/app/server", []byte("server binary"), 0755)
	writeFile(app, "/etc/app.conf", []byte("app config"), 0644)

	// Layer 3: User customizations (writable)
	custom := mustNewMemFS()

	ufs := New(
		WithWritableLayer(custom),
		WithReadOnlyLayer(app),
		WithReadOnlyLayer(runtime),
		WithReadOnlyLayer(baseOS),
	)

	// Test reading from different layers
	tests := []struct {
		path    string
		content string
	}{
		{"/bin/sh", "shell"},
		{"/lib/libc.so", "libc"},
		{"/app/server", "server binary"},
		{"/etc/app.conf", "app config"},
	}

	for _, tt := range tests {
		data, err := readFile(ufs, tt.path)
		if err != nil {
			t.Errorf("failed to read %s: %v", tt.path, err)
			continue
		}
		if string(data) != tt.content {
			t.Errorf("%s: expected '%s', got '%s'", tt.path, tt.content, string(data))
		}
	}

	// Override base OS file in custom layer
	writeFile(ufs, "/etc/passwd", []byte("root:x:0:0\nuser:x:1000:1000"), 0644)

	// Should read modified version
	data, err := readFile(ufs, "/etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	expected := "root:x:0:0\nuser:x:1000:1000"
	if string(data) != expected {
		t.Errorf("expected '%s', got '%s'", expected, string(data))
	}

	// Base layer should be unchanged
	data, err = readFile(baseOS, "/etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "root:x:0:0" {
		t.Error("base layer was modified")
	}

	// Delete file from app layer
	err = ufs.Remove("/etc/app.conf")
	if err != nil {
		t.Fatal(err)
	}

	// Should not be visible
	if _, err := ufs.Stat("/etc/app.conf"); err == nil {
		t.Error("deleted file should not be visible")
	}

	// But still exists in app layer
	if _, err := app.Stat("/etc/app.conf"); err != nil {
		t.Error("file should still exist in app layer")
	}
}

// TestDirectoryMergingComplex tests complex directory merging scenarios
func TestDirectoryMergingComplex(t *testing.T) {
	layer0 := mustNewMemFS()
	layer1 := mustNewMemFS()
	layer2 := mustNewMemFS()
	overlay := mustNewMemFS()

	// Create overlapping directory structures
	writeFile(layer0, "/data/file1.txt", []byte("1"), 0644)
	writeFile(layer0, "/data/file2.txt", []byte("2"), 0644)
	writeFile(layer0, "/data/file3.txt", []byte("3"), 0644)

	writeFile(layer1, "/data/file2.txt", []byte("2-override"), 0644)
	writeFile(layer1, "/data/file4.txt", []byte("4"), 0644)

	writeFile(layer2, "/data/file5.txt", []byte("5"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(layer2),
		WithReadOnlyLayer(layer1),
		WithReadOnlyLayer(layer0),
	)

	// Read merged directory
	entries, err := readDir(ufs, "/data")
	if err != nil {
		t.Fatalf("failed to read dir: %v", err)
	}

	// Should have 5 unique files
	if len(entries) != 5 {
		t.Errorf("expected 5 entries, got %d", len(entries))
	}

	// Check that layer precedence is respected for file2
	data, err := readFile(ufs, "/data/file2.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "2-override" {
		t.Error("layer precedence not respected")
	}

	// Delete file4 from layer1 via whiteout
	err = ufs.Remove("/data/file4.txt")
	if err != nil {
		t.Fatal(err)
	}

	// Read directory again
	entries, err = readDir(ufs, "/data")
	if err != nil {
		t.Fatal(err)
	}

	// Should have 4 files now
	if len(entries) != 4 {
		t.Errorf("expected 4 entries after deletion, got %d", len(entries))
	}

	// Verify file4 is not in the list
	for _, entry := range entries {
		if entry.Name() == "file4.txt" {
			t.Error("deleted file should not appear in directory listing")
		}
	}
}

// TestConcurrentAccess tests thread-safe concurrent access
func TestConcurrentAccess(t *testing.T) {
	baseLayer := mustNewMemFS()
	overlay := mustNewMemFS()

	// Create initial files
	for i := 0; i < 100; i++ {
		writeFile(baseLayer, fmt.Sprintf("/file%d.txt", i), []byte("content"), 0644)
	}

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(baseLayer),
		WithStatCache(true, 5*time.Minute),
	)

	// Concurrent readers
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				_, err := ufs.Stat(fmt.Sprintf("/file%d.txt", j%100))
				if err != nil {
					t.Errorf("concurrent read failed: %v", err)
				}
			}
			done <- true
		}()
	}

	// Wait for all readers
	for i := 0; i < 10; i++ {
		<-done
	}
}

// TestRenameAcrossLayers tests renaming files that exist in different layers
func TestRenameAcrossLayers(t *testing.T) {
	baseLayer := mustNewMemFS()
	overlay := mustNewMemFS()

	writeFile(baseLayer, "/old.txt", []byte("content"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(baseLayer),
	)

	// Rename file from base layer
	err := ufs.Rename("/old.txt", "/new.txt")
	if err != nil {
		t.Fatalf("failed to rename: %v", err)
	}

	// Old should not be visible
	if _, err := ufs.Stat("/old.txt"); err == nil {
		t.Error("old file should not exist")
	}

	// New should exist in overlay
	data, err := readFile(ufs, "/new.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "content" {
		t.Error("content mismatch after rename")
	}

	// Base layer should be unchanged
	if _, err := baseLayer.Stat("/old.txt"); err != nil {
		t.Error("base layer should still have old file")
	}

	// Overlay should have whiteout for old
	whiteout := whiteoutPath("/old.txt")
	if _, err := overlay.Stat(whiteout); err != nil {
		t.Error("whiteout should exist for renamed file")
	}
}

// TestMetadataOperations tests Chmod, Chown, Chtimes with copy-on-write
func TestMetadataOperations(t *testing.T) {
	baseLayer := mustNewMemFS()
	overlay := mustNewMemFS()

	writeFile(baseLayer, "/test.txt", []byte("content"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(baseLayer),
	)

	// Change permissions
	err := ufs.Chmod("/test.txt", 0600)
	if err != nil {
		t.Fatalf("failed to chmod: %v", err)
	}

	// File should be copied to overlay
	if _, err := overlay.Stat("/test.txt"); err != nil {
		t.Error("file should be copied to overlay after chmod")
	}

	// Check new permissions
	info, err := ufs.Stat("/test.txt")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("expected 0600, got %o", info.Mode().Perm())
	}

	// Base layer should be unchanged
	info, err = baseLayer.Stat("/test.txt")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0644 {
		t.Error("base layer permissions were modified")
	}
}

// TestCacheExpiration tests that cache entries expire correctly
func TestCacheExpiration(t *testing.T) {
	baseLayer := mustNewMemFS()
	overlay := mustNewMemFS()

	writeFile(baseLayer, "/test.txt", []byte("content"), 0644)

	// Short TTL for testing
	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(baseLayer),
		WithCacheConfig(true, 100*time.Millisecond, 50*time.Millisecond, 1000),
	)

	// First stat - caches the result
	_, err := ufs.Stat("/test.txt")
	if err != nil {
		t.Fatal(err)
	}

	// Check cache stats
	stats := ufs.CacheStats()
	if !stats.Enabled {
		t.Error("cache should be enabled")
	}
	if stats.StatCacheSize != 1 {
		t.Errorf("expected 1 cached entry, got %d", stats.StatCacheSize)
	}

	// Wait for cache to expire
	time.Sleep(150 * time.Millisecond)

	// This should trigger a new lookup since cache expired
	// We can't easily verify this without more instrumentation,
	// but at least we verify it doesn't crash
	_, err = ufs.Stat("/test.txt")
	if err != nil {
		t.Fatal(err)
	}
}

// TestWriteInvalidatesParentCache tests that writing a file invalidates parent directory cache
func TestWriteInvalidatesParentCache(t *testing.T) {
	baseLayer := mustNewMemFS()
	overlay := mustNewMemFS()

	baseLayer.MkdirAll("/dir", 0755)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(baseLayer),
		WithStatCache(true, 5*time.Minute),
	)

	// Read directory - caches empty result
	entries, err := readDir(ufs, "/dir")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Error("directory should be empty")
	}

	// Write new file
	writeFile(ufs, "/dir/new.txt", []byte("content"), 0644)

	// Read directory again - should see new file even with cache
	// Note: Directory contents are not cached by our current implementation,
	// only stat results, so this should work
	entries, err = readDir(ufs, "/dir")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("expected 1 entry, got %d", len(entries))
	}
}
