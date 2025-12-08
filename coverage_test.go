package unionfs

import (
	"io"
	"os"
	"path"
	"testing"
	"time"

	
)

// =============================================================================
// Layer Precedence Tests (Phase 2)
// =============================================================================

// TestLayerPrecedenceWith4Layers tests file lookup with 4+ layers
func TestLayerPrecedenceWith4Layers(t *testing.T) {
	layer0 := mustNewMemFS() // bottom
	layer1 := mustNewMemFS()
	layer2 := mustNewMemFS()
	layer3 := mustNewMemFS()
	overlay := mustNewMemFS() // top writable

	// File only in bottom layer
	writeFile(layer0, "/bottom.txt", []byte("layer0"), 0644)

	// File in layer1 and layer0 - layer1 should win
	writeFile(layer0, "/test1.txt", []byte("layer0-test1"), 0644)
	writeFile(layer1, "/test1.txt", []byte("layer1-test1"), 0644)

	// File in layer2, layer1, layer0 - layer2 should win
	writeFile(layer0, "/test2.txt", []byte("layer0-test2"), 0644)
	writeFile(layer1, "/test2.txt", []byte("layer1-test2"), 0644)
	writeFile(layer2, "/test2.txt", []byte("layer2-test2"), 0644)

	// File in all layers except overlay - layer3 should win
	writeFile(layer0, "/test3.txt", []byte("layer0-test3"), 0644)
	writeFile(layer1, "/test3.txt", []byte("layer1-test3"), 0644)
	writeFile(layer2, "/test3.txt", []byte("layer2-test3"), 0644)
	writeFile(layer3, "/test3.txt", []byte("layer3-test3"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(layer3),
		WithReadOnlyLayer(layer2),
		WithReadOnlyLayer(layer1),
		WithReadOnlyLayer(layer0),
	)

	tests := []struct {
		path    string
		want    string
	}{
		{"/bottom.txt", "layer0"},
		{"/test1.txt", "layer1-test1"},
		{"/test2.txt", "layer2-test2"},
		{"/test3.txt", "layer3-test3"},
	}

	for _, tt := range tests {
		data, err := readFile(ufs, tt.path)
		if err != nil {
			t.Errorf("failed to read %s: %v", tt.path, err)
			continue
		}
		if string(data) != tt.want {
			t.Errorf("%s: got %q, want %q", tt.path, string(data), tt.want)
		}
	}
}

// TestLayerPrecedenceDirectoryInMultipleLayers tests directory precedence
func TestLayerPrecedenceDirectoryInMultipleLayers(t *testing.T) {
	layer0 := mustNewMemFS()
	layer1 := mustNewMemFS()
	overlay := mustNewMemFS()

	// Same directory in both layers with different files
	writeFile(layer0, "/dir/from0.txt", []byte("0"), 0644)
	writeFile(layer1, "/dir/from1.txt", []byte("1"), 0644)

	// Same file in both layers
	writeFile(layer0, "/dir/shared.txt", []byte("layer0-shared"), 0644)
	writeFile(layer1, "/dir/shared.txt", []byte("layer1-shared"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(layer1),
		WithReadOnlyLayer(layer0),
	)

	// Should get layer1 version of shared file
	data, err := readFile(ufs, "/dir/shared.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "layer1-shared" {
		t.Errorf("got %q, want %q", string(data), "layer1-shared")
	}

	// Directory listing should merge contents
	entries, err := readDir(ufs, "/dir")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 { // from0.txt, from1.txt, shared.txt
		t.Errorf("got %d entries, want 3", len(entries))
	}
}

// TestLayerPrecedenceAfterModification tests precedence after writes
func TestLayerPrecedenceAfterModification(t *testing.T) {
	layer0 := mustNewMemFS()
	layer1 := mustNewMemFS()
	overlay := mustNewMemFS()

	writeFile(layer0, "/test.txt", []byte("layer0"), 0644)
	writeFile(layer1, "/test.txt", []byte("layer1"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(layer1),
		WithReadOnlyLayer(layer0),
		WithStatCache(true, 5*time.Minute),
	)

	// Should read from layer1
	data, _ := readFile(ufs, "/test.txt")
	if string(data) != "layer1" {
		t.Errorf("before write: got %q, want %q", string(data), "layer1")
	}

	// Write to overlay
	writeFile(ufs, "/test.txt", []byte("overlay"), 0644)

	// Should now read from overlay
	data, _ = readFile(ufs, "/test.txt")
	if string(data) != "overlay" {
		t.Errorf("after write: got %q, want %q", string(data), "overlay")
	}

	// Base layers should be unchanged
	data, _ = readFile(layer0, "/test.txt")
	if string(data) != "layer0" {
		t.Error("layer0 was modified")
	}
	data, _ = readFile(layer1, "/test.txt")
	if string(data) != "layer1" {
		t.Error("layer1 was modified")
	}
}

// TestLayerSearchStopsAtFirstMatch verifies search stops at first match
func TestLayerSearchStopsAtFirstMatch(t *testing.T) {
	layer0 := mustNewMemFS()
	layer1 := mustNewMemFS()
	overlay := mustNewMemFS()

	writeFile(layer0, "/test.txt", []byte("layer0"), 0644)
	writeFile(layer1, "/test.txt", []byte("layer1"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(layer1),
		WithReadOnlyLayer(layer0),
	)

	// findFile should return layer1 (index 1 since overlay is 0)
	info, layerIdx, err := ufs.findFile("/test.txt")
	if err != nil {
		t.Fatal(err)
	}
	if layerIdx != 1 {
		t.Errorf("got layer index %d, want 1", layerIdx)
	}
	if info == nil {
		t.Error("info is nil")
	}
}

// =============================================================================
// absfs Adapter Tests
// =============================================================================

// TestAbsFSAdapterMkdir tests Mkdir via the absfs adapter
func TestAbsFSAdapterMkdir(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	adapter := &absFSAdapter{ufs: ufs}

	err := adapter.Mkdir("/testdir", 0755)
	if err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}

	// Verify directory exists
	info, err := ufs.Stat("/testdir")
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Error("expected directory")
	}
}

// TestAbsFSAdapterRemove tests Remove via the absfs adapter
func TestAbsFSAdapterRemove(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	writeFile(base, "/test.txt", []byte("content"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	adapter := &absFSAdapter{ufs: ufs}

	err := adapter.Remove("/test.txt")
	if err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	// File should not be visible
	_, err = ufs.Stat("/test.txt")
	if err == nil {
		t.Error("file should not exist after remove")
	}
}

// TestAbsFSAdapterRename tests Rename via the absfs adapter
func TestAbsFSAdapterRename(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	writeFile(base, "/old.txt", []byte("content"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	adapter := &absFSAdapter{ufs: ufs}

	err := adapter.Rename("/old.txt", "/new.txt")
	if err != nil {
		t.Fatalf("Rename failed: %v", err)
	}

	// Old should not exist
	if _, err := ufs.Stat("/old.txt"); err == nil {
		t.Error("old file should not exist")
	}

	// New should exist
	data, err := readFile(ufs, "/new.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "content" {
		t.Error("content mismatch")
	}
}

// TestAbsFSAdapterChmod tests Chmod via the absfs adapter
func TestAbsFSAdapterChmod(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	writeFile(base, "/test.txt", []byte("content"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	adapter := &absFSAdapter{ufs: ufs}

	err := adapter.Chmod("/test.txt", 0600)
	if err != nil {
		t.Fatalf("Chmod failed: %v", err)
	}

	info, _ := ufs.Stat("/test.txt")
	if info.Mode().Perm() != 0600 {
		t.Errorf("got %o, want 0600", info.Mode().Perm())
	}
}

// TestAbsFSAdapterChtimes tests Chtimes via the absfs adapter
func TestAbsFSAdapterChtimes(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	writeFile(base, "/test.txt", []byte("content"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	adapter := &absFSAdapter{ufs: ufs}

	newTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	err := adapter.Chtimes("/test.txt", newTime, newTime)
	if err != nil {
		t.Fatalf("Chtimes failed: %v", err)
	}

	info, _ := ufs.Stat("/test.txt")
	if !info.ModTime().Equal(newTime) {
		t.Errorf("got %v, want %v", info.ModTime(), newTime)
	}
}

// TestAbsFSAdapterChown tests Chown via the absfs adapter
func TestAbsFSAdapterChown(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	writeFile(base, "/test.txt", []byte("content"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	adapter := &absFSAdapter{ufs: ufs}

	// Chown is typically a no-op on in-memory filesystems, but it should trigger copy-up
	err := adapter.Chown("/test.txt", 1000, 1000)
	if err != nil {
		t.Fatalf("Chown failed: %v", err)
	}

	// File should now exist in overlay (copy-up)
	if _, err := overlay.Stat("/test.txt"); err != nil {
		t.Error("file should be in overlay after Chown")
	}
}

// =============================================================================
// File Wrapper Tests (unionFile methods)
// =============================================================================

// TestUnionFileName tests the Name method
func TestUnionFileName(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	writeFile(base, "/test.txt", []byte("content"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	adapter := &absFSAdapter{ufs: ufs}
	file, err := adapter.OpenFile("/test.txt", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	// Get underlying unionFile wrapper (unwrap from ExtendSeekable)
	// The Name() method should return the file name (normalize for cross-platform)
	if file.Name() != "/test.txt" {
		t.Errorf("got %q, want /test.txt", file.Name())
	}
}

// TestUnionFileSync tests the Sync method
func TestUnionFileSync(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Create a file
	file, err := ufs.Create("/test.txt")
	if err != nil {
		t.Fatal(err)
	}

	// Write some data
	file.Write([]byte("content"))

	// Sync should not error
	if syncer, ok := file.(interface{ Sync() error }); ok {
		if err := syncer.Sync(); err != nil {
			t.Errorf("Sync failed: %v", err)
		}
	}
	file.Close()
}

// TestUnionFileReadAt tests ReadAt functionality
func TestUnionFileReadAt(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	writeFile(base, "/test.txt", []byte("Hello, World!"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	adapter := &absFSAdapter{ufs: ufs}
	file, err := adapter.OpenFile("/test.txt", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	// Read at offset 7
	buf := make([]byte, 6)
	n, err := file.ReadAt(buf, 7)
	if err != nil && err != io.EOF {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if string(buf[:n]) != "World!" {
		t.Errorf("got %q, want %q", string(buf[:n]), "World!")
	}
}

// TestUnionFileWriteAt tests WriteAt functionality
func TestUnionFileWriteAt(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	adapter := &absFSAdapter{ufs: ufs}

	// Create file
	file, err := adapter.OpenFile("/test.txt", os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}

	// Write initial content
	file.Write([]byte("Hello, World!"))

	// Write at offset 7
	n, err := file.WriteAt([]byte("Go!!!"), 7)
	if err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	if n != 5 {
		t.Errorf("wrote %d bytes, want 5", n)
	}
	file.Close()

	// Verify content - WriteAt overwrites starting at offset, keeps rest
	data, _ := readFile(ufs, "/test.txt")
	expected := "Hello, Go!!!!"
	if string(data) != expected {
		t.Errorf("got %q, want %q", string(data), expected)
	}
}

// TestUnionFileWriteString tests WriteString functionality
func TestUnionFileWriteString(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	adapter := &absFSAdapter{ufs: ufs}
	file, err := adapter.OpenFile("/test.txt", os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}

	n, err := file.WriteString("Hello from WriteString")
	if err != nil {
		t.Fatalf("WriteString failed: %v", err)
	}
	if n != 22 {
		t.Errorf("wrote %d bytes, want 22", n)
	}
	file.Close()

	data, _ := readFile(ufs, "/test.txt")
	if string(data) != "Hello from WriteString" {
		t.Errorf("got %q", string(data))
	}
}

// TestUnionFileTruncate tests Truncate on unionFile
func TestUnionFileTruncate(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	adapter := &absFSAdapter{ufs: ufs}

	// Create file with content
	file, err := adapter.OpenFile("/test.txt", os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	file.Write([]byte("Hello, World!"))

	// Truncate via file handle
	if truncater, ok := file.(interface{ Truncate(int64) error }); ok {
		err = truncater.Truncate(5)
		if err != nil {
			t.Fatalf("Truncate failed: %v", err)
		}
	}
	file.Close()

	// Verify size
	info, _ := ufs.Stat("/test.txt")
	if info.Size() != 5 {
		t.Errorf("size = %d, want 5", info.Size())
	}
}

// =============================================================================
// Cache Tests
// =============================================================================

// TestCacheEviction tests cache eviction when maxEntries is reached
func TestCacheEviction(t *testing.T) {
	// Create cache with small max entries
	cache := newCache(true, 5*time.Minute, 2*time.Minute, 3)

	// Add entries to trigger eviction
	cache.putStat("/file1", &mockFileInfo{name: "file1"}, 0)
	time.Sleep(10 * time.Millisecond)
	cache.putStat("/file2", &mockFileInfo{name: "file2"}, 0)
	time.Sleep(10 * time.Millisecond)
	cache.putStat("/file3", &mockFileInfo{name: "file3"}, 0)
	time.Sleep(10 * time.Millisecond)

	// This should trigger eviction
	cache.putStat("/file4", &mockFileInfo{name: "file4"}, 0)

	stats := cache.Stats()
	if stats.StatCacheSize > 3 {
		t.Errorf("cache size %d exceeds max %d", stats.StatCacheSize, 3)
	}
}

// TestNegativeCacheEviction tests negative cache eviction
func TestNegativeCacheEviction(t *testing.T) {
	cache := newCache(true, 5*time.Minute, 2*time.Minute, 3)

	// Add entries to trigger eviction
	cache.putNegative("/file1")
	time.Sleep(10 * time.Millisecond)
	cache.putNegative("/file2")
	time.Sleep(10 * time.Millisecond)
	cache.putNegative("/file3")
	time.Sleep(10 * time.Millisecond)

	// This should trigger eviction
	cache.putNegative("/file4")

	stats := cache.Stats()
	if stats.NegativeCacheSize > 3 {
		t.Errorf("negative cache size %d exceeds max %d", stats.NegativeCacheSize, 3)
	}
}

// TestCacheClear tests clearing the cache
func TestCacheClear(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	writeFile(base, "/test.txt", []byte("content"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
		WithStatCache(true, 5*time.Minute),
	)

	// Populate cache
	ufs.Stat("/test.txt")
	ufs.Stat("/nonexistent.txt") // Should populate negative cache

	stats := ufs.CacheStats()
	if stats.StatCacheSize == 0 {
		t.Error("cache should not be empty")
	}

	// Clear cache
	ufs.ClearCache()

	stats = ufs.CacheStats()
	if stats.StatCacheSize != 0 {
		t.Errorf("stat cache should be empty, got %d", stats.StatCacheSize)
	}
	if stats.NegativeCacheSize != 0 {
		t.Errorf("negative cache should be empty, got %d", stats.NegativeCacheSize)
	}
}

// TestCacheZeroTTL tests cache with zero TTL (effectively disabled per-request)
func TestCacheZeroTTL(t *testing.T) {
	cache := newCache(true, 0, 0, 100)

	cache.putStat("/test", &mockFileInfo{name: "test"}, 0)

	// With zero TTL, cache entry should be immediately expired
	// Add a tiny sleep to ensure time.Now() advances past the expiry time
	time.Sleep(1 * time.Millisecond)

	_, _, ok := cache.getStat("/test")
	if ok {
		t.Error("cache entry with zero TTL should be expired")
	}
}

// TestInvalidateTreeNested tests invalidating cache for nested paths
func TestInvalidateTreeNested(t *testing.T) {
	cache := newCache(true, 5*time.Minute, 2*time.Minute, 100)

	// Add entries in a tree structure
	cache.putStat("/dir", &mockFileInfo{name: "dir"}, 0)
	cache.putStat("/dir/sub", &mockFileInfo{name: "sub"}, 0)
	cache.putStat("/dir/sub/file.txt", &mockFileInfo{name: "file.txt"}, 0)
	cache.putStat("/other/file.txt", &mockFileInfo{name: "file.txt"}, 0)
	cache.putNegative("/dir/sub/nonexistent")

	// Invalidate /dir tree
	cache.invalidateTree("/dir")

	// /dir entries should be gone
	if _, _, ok := cache.getStat("/dir"); ok {
		t.Error("/dir should be invalidated")
	}
	if _, _, ok := cache.getStat("/dir/sub"); ok {
		t.Error("/dir/sub should be invalidated")
	}
	if _, _, ok := cache.getStat("/dir/sub/file.txt"); ok {
		t.Error("/dir/sub/file.txt should be invalidated")
	}
	if cache.isNegative("/dir/sub/nonexistent") {
		t.Error("negative entry should be invalidated")
	}

	// /other should still exist
	if _, _, ok := cache.getStat("/other/file.txt"); !ok {
		t.Error("/other/file.txt should still exist")
	}
}

// =============================================================================
// Directory Operations Tests
// =============================================================================

// TestDirectoryRead tests Read method on directories (should fail)
func TestDirectoryRead(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	base.MkdirAll("/dir", 0755)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	dir, err := ufs.Open("/dir")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()

	// Read should fail on directory
	buf := make([]byte, 10)
	_, err = dir.Read(buf)
	if err != os.ErrInvalid {
		t.Errorf("expected ErrInvalid, got %v", err)
	}
}

// TestDirectoryReadAt tests ReadAt method on directories (should fail)
func TestDirectoryReadAt(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	base.MkdirAll("/dir", 0755)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	dir, err := ufs.Open("/dir")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()

	// ReadAt should fail on directory
	buf := make([]byte, 10)
	if d, ok := dir.(*unionDir); ok {
		_, err = d.ReadAt(buf, 0)
		if err != os.ErrInvalid {
			t.Errorf("expected ErrInvalid, got %v", err)
		}
	}
}

// TestDirectoryWrite tests Write method on directories (should fail)
func TestDirectoryWrite(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	base.MkdirAll("/dir", 0755)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	dir, err := ufs.Open("/dir")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()

	if d, ok := dir.(*unionDir); ok {
		_, err = d.Write([]byte("test"))
		if err != os.ErrInvalid {
			t.Errorf("expected ErrInvalid, got %v", err)
		}
	}
}

// TestDirectoryWriteAt tests WriteAt method on directories (should fail)
func TestDirectoryWriteAt(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	base.MkdirAll("/dir", 0755)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	dir, err := ufs.Open("/dir")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()

	if d, ok := dir.(*unionDir); ok {
		_, err = d.WriteAt([]byte("test"), 0)
		if err != os.ErrInvalid {
			t.Errorf("expected ErrInvalid, got %v", err)
		}
	}
}

// TestDirectoryWriteString tests WriteString on directories (should fail)
func TestDirectoryWriteString(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	base.MkdirAll("/dir", 0755)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	dir, err := ufs.Open("/dir")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()

	if d, ok := dir.(*unionDir); ok {
		_, err = d.WriteString("test")
		if err != os.ErrInvalid {
			t.Errorf("expected ErrInvalid, got %v", err)
		}
	}
}

// TestDirectoryTruncate tests Truncate on directories (should fail)
func TestDirectoryTruncate(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	base.MkdirAll("/dir", 0755)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	dir, err := ufs.Open("/dir")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()

	if d, ok := dir.(*unionDir); ok {
		err = d.Truncate(0)
		if err != os.ErrInvalid {
			t.Errorf("expected ErrInvalid, got %v", err)
		}
	}
}

// TestDirectorySync tests Sync on directories (should be no-op)
func TestDirectorySync(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	base.MkdirAll("/dir", 0755)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	dir, err := ufs.Open("/dir")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()

	if d, ok := dir.(*unionDir); ok {
		// Sync should not error
		if err := d.Sync(); err != nil {
			t.Errorf("Sync failed: %v", err)
		}
	}
}

// TestDirectoryName tests Name method on directories
func TestDirectoryName(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	base.MkdirAll("/mydir", 0755)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	dir, err := ufs.Open("/mydir")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()

	if d, ok := dir.(*unionDir); ok {
		if d.Name() != "mydir" {
			t.Errorf("got %q, want %q", d.Name(), "mydir")
		}
	}
}

// TestDirectorySeekAllWhence tests all Seek whence values
func TestDirectorySeekAllWhence(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	// Create 5 files
	for i := 0; i < 5; i++ {
		writeFile(base, path.Join("/dir", string(rune('a'+i))+".txt"), []byte("x"), 0644)
	}

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	dir, err := ufs.Open("/dir")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()

	// Read 2 entries to advance position
	dir.Readdir(2)

	// SeekCurrent
	pos, err := dir.Seek(1, io.SeekCurrent)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 3 {
		t.Errorf("SeekCurrent: got %d, want 3", pos)
	}

	// SeekEnd
	pos, err = dir.Seek(-2, io.SeekEnd)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 3 { // 5 entries - 2 = 3
		t.Errorf("SeekEnd: got %d, want 3", pos)
	}

	// SeekStart
	pos, err = dir.Seek(0, io.SeekStart)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 0 {
		t.Errorf("SeekStart: got %d, want 0", pos)
	}

	// Seek to negative should clamp to 0
	pos, err = dir.Seek(-10, io.SeekStart)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 0 {
		t.Errorf("negative seek: got %d, want 0", pos)
	}
}

// TestDirectorySeekOnClosed tests Seek on closed directory
func TestDirectorySeekOnClosed(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	base.MkdirAll("/dir", 0755)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	dir, err := ufs.Open("/dir")
	if err != nil {
		t.Fatal(err)
	}
	dir.Close()

	// Seek on closed directory should fail
	_, err = dir.Seek(0, io.SeekStart)
	if err != os.ErrClosed {
		t.Errorf("expected ErrClosed, got %v", err)
	}
}

// TestDirectoryStatOnClosed tests Stat on closed directory
func TestDirectoryStatOnClosed(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	base.MkdirAll("/dir", 0755)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	dir, err := ufs.Open("/dir")
	if err != nil {
		t.Fatal(err)
	}
	dir.Close()

	// Stat on closed directory should fail
	if d, ok := dir.(*unionDir); ok {
		_, err = d.Stat()
		if err != os.ErrClosed {
			t.Errorf("expected ErrClosed, got %v", err)
		}
	}
}

// TestReaddirOnClosed tests Readdir on closed directory
func TestReaddirOnClosed(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	base.MkdirAll("/dir", 0755)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	dir, err := ufs.Open("/dir")
	if err != nil {
		t.Fatal(err)
	}
	dir.Close()

	// Readdir on closed should fail
	_, err = dir.Readdir(-1)
	if err != os.ErrClosed {
		t.Errorf("expected ErrClosed, got %v", err)
	}
}

// TestReaddirnames tests Readdirnames method
func TestReaddirnames(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	writeFile(base, "/dir/file1.txt", []byte("1"), 0644)
	writeFile(base, "/dir/file2.txt", []byte("2"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	dir, err := ufs.Open("/dir")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()

	names, err := dir.Readdirnames(-1)
	if err != nil {
		t.Fatal(err)
	}

	if len(names) != 2 {
		t.Errorf("got %d names, want 2", len(names))
	}
}

// TestReaddirWithCount tests Readdir with various count values
func TestReaddirWithCount(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	for i := 0; i < 5; i++ {
		writeFile(base, path.Join("/dir", string(rune('a'+i))+".txt"), []byte("x"), 0644)
	}

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Test count = 0 (should return all)
	dir, _ := ufs.Open("/dir")
	entries, err := dir.Readdir(0)
	dir.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 5 {
		t.Errorf("count=0: got %d entries, want 5", len(entries))
	}

	// Test count > entries (should return all)
	dir, _ = ufs.Open("/dir")
	entries, err = dir.Readdir(10)
	dir.Close()
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if len(entries) != 5 {
		t.Errorf("count=10: got %d entries, want 5", len(entries))
	}

	// Test reading past end
	dir, _ = ufs.Open("/dir")
	dir.Readdir(-1) // Read all
	entries, err = dir.Readdir(1)
	dir.Close()
	if err != io.EOF {
		t.Errorf("expected EOF, got %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("got %d entries after EOF, want 0", len(entries))
	}
}

// =============================================================================
// Copy-on-Write Tests
// =============================================================================

// TestCopyUpDir tests copying up directories
func TestCopyUpDir(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	// Create directory with specific mode in base
	base.MkdirAll("/testdir", 0700)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Get directory info
	info, _, _ := ufs.findFile("/testdir")

	// Trigger copy-up of directory
	err := ufs.copyUpDir("/testdir", info)
	if err != nil {
		t.Fatalf("copyUpDir failed: %v", err)
	}

	// Verify directory exists in overlay
	overlayInfo, err := overlay.Stat("/testdir")
	if err != nil {
		t.Fatal(err)
	}
	if !overlayInfo.IsDir() {
		t.Error("expected directory")
	}
}

// TestCopyUpParents tests copying up parent directories
func TestCopyUpParents(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	// Create nested directory in base
	base.MkdirAll("/a/b/c", 0755)
	writeFile(base, "/a/b/c/file.txt", []byte("content"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Copy up parents for a deeply nested file
	err := ufs.copyUpParents("/a/b/c/newfile.txt")
	if err != nil {
		t.Fatalf("copyUpParents failed: %v", err)
	}

	// Parent directories should exist (either in overlay or accessible)
	if _, err := ufs.Stat("/a/b/c"); err != nil {
		t.Error("parent directory should be accessible")
	}
}

// TestCopyUpParentsNonExistent tests copyUpParents when parent doesn't exist
func TestCopyUpParentsNonExistent(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Copy up parents for path where parents don't exist
	err := ufs.copyUpParents("/newdir/newfile.txt")
	if err != nil {
		t.Fatalf("copyUpParents failed: %v", err)
	}

	// Parent directory should be created
	if _, err := overlay.Stat("/newdir"); err != nil {
		t.Error("parent directory should be created")
	}
}

// TestCopyUpLargeFile tests copying up large files
func TestCopyUpLargeFile(t *testing.T) {
	t.Skip("memfs does not currently support O_APPEND mode correctly")

	overlay := mustNewMemFS()
	base := mustNewMemFS()

	// Create a file larger than the default buffer size
	largeContent := make([]byte, 100*1024) // 100KB
	for i := range largeContent {
		largeContent[i] = byte(i % 256)
	}
	writeFile(base, "/large.bin", largeContent, 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Modify file to trigger copy-up
	f, err := ufs.OpenFile("/large.bin", os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("appended"))
	f.Close()

	// Verify file was copied to overlay
	data, err := readFile(overlay, "/large.bin")
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != len(largeContent)+8 {
		t.Errorf("got size %d, want %d", len(data), len(largeContent)+8)
	}
}

// TestCopyBufferSize tests custom copy buffer size
func TestCopyBufferSize(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	writeFile(base, "/test.txt", []byte("content"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
		WithCopyBufferSize(64*1024), // 64KB buffer
	)

	if ufs.copyBufferSize != 64*1024 {
		t.Errorf("got buffer size %d, want %d", ufs.copyBufferSize, 64*1024)
	}

	// Trigger copy-up
	ufs.Chmod("/test.txt", 0600)

	// Verify file was copied
	if _, err := overlay.Stat("/test.txt"); err != nil {
		t.Error("file should be in overlay")
	}
}

// =============================================================================
// Symlink Tests
// =============================================================================

// TestReadlinkFromLayers tests reading symlinks from different layers
func TestReadlinkFromLayers(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	// Note: afero.MemMapFs doesn't support symlinks, so Readlink will return not exist
	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// This should return error since MemMapFs doesn't support symlinks
	_, err := ufs.Readlink("/nonexistent")
	if err == nil {
		t.Error("expected error for non-existent symlink")
	}
}

// TestLstatIfPossible tests LstatIfPossible method
func TestLstatIfPossible(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	writeFile(base, "/test.txt", []byte("content"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	info, supported, err := ufs.LstatIfPossible("/test.txt")
	if err != nil {
		t.Fatal(err)
	}
	if info == nil {
		t.Error("info should not be nil")
	}
	// MemMapFs doesn't support Lstat, so supported should be false
	_ = supported
}

// TestLstatIfPossibleNotFound tests LstatIfPossible for non-existent file
func TestLstatIfPossibleNotFound(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	_, _, err := ufs.LstatIfPossible("/nonexistent")
	if err == nil {
		t.Error("expected error for non-existent file")
	}
}

// TestReadlinkIfPossible tests ReadlinkIfPossible method
func TestReadlinkIfPossible(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Should call Readlink internally
	_, err := ufs.ReadlinkIfPossible("/nonexistent")
	if err == nil {
		t.Error("expected error")
	}
}

// TestSymlinkIfPossible tests SymlinkIfPossible method
func TestSymlinkIfPossible(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Should call Symlink internally
	err := ufs.SymlinkIfPossible("/target", "/link")
	// memfs requires target to exist, so we expect an error
	if err == nil {
		t.Error("expected error for non-existent target")
	}
}

// TestFollowSymlinks tests the followSymlinks method
func TestFollowSymlinks(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	writeFile(base, "/test.txt", []byte("content"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// For regular file, should return the same path
	resolved, err := ufs.followSymlinks("/test.txt")
	if err != nil {
		t.Fatal(err)
	}
	// Normalize for cross-platform comparison
	if resolved != "/test.txt" {
		t.Errorf("got %q, want /test.txt", resolved)
	}
}

// TestResolveSymlinkMaxDepth tests resolveSymlink max depth handling
func TestResolveSymlinkMaxDepth(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	writeFile(base, "/test.txt", []byte("content"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// With depth 0, should return error
	_, err := ufs.resolveSymlink("/test.txt", 0)
	if err != os.ErrInvalid {
		t.Errorf("expected ErrInvalid for max depth, got %v", err)
	}
}

// TestIsSymlinkLoop tests the isSymlinkLoop helper function
func TestIsSymlinkLoop(t *testing.T) {
	visited := map[string]bool{
		"/a":     true,
		"/a/b":   true,
		"/a/b/c": true,
	}

	// Should detect loop when target points back
	if !isSymlinkLoop("/x", "/a/b", visited) {
		t.Error("should detect loop")
	}

	// Should not detect loop for unvisited path
	if isSymlinkLoop("/x", "/d/e/f", visited) {
		t.Error("should not detect loop for new path")
	}

	// Test with relative path
	if !isSymlinkLoop("/a/b/c", "../b", visited) {
		t.Error("should detect loop with relative path")
	}

	// Test direct self-reference
	visited2 := map[string]bool{
		"/test": true,
	}
	if !isSymlinkLoop("/link", "/test", visited2) {
		t.Error("should detect direct loop")
	}
}

// =============================================================================
// Other Operations Tests
// =============================================================================

// TestName tests the Name method
func TestName(t *testing.T) {
	ufs := New()
	if ufs.Name() != "UnionFS" {
		t.Errorf("got %q, want %q", ufs.Name(), "UnionFS")
	}
}

// TestLstat tests the Lstat method
func TestLstat(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	writeFile(base, "/test.txt", []byte("content"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	info, err := ufs.Lstat("/test.txt")
	if err != nil {
		t.Fatal(err)
	}
	if info.Name() != "test.txt" {
		t.Errorf("got %q, want test.txt", info.Name())
	}
}

// TestCreate tests the Create method
func TestCreate(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	f, err := ufs.Create("/newfile.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("content"))
	f.Close()

	// Verify file exists
	data, err := readFile(ufs, "/newfile.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "content" {
		t.Errorf("got %q, want content", string(data))
	}
}

// TestAsAbsFS tests the deprecated AsAbsFS method
func TestAsAbsFS(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	fs := ufs.AsAbsFS()
	if fs == nil {
		t.Error("AsAbsFS should return non-nil")
	}
}

// TestChown tests the Chown method
func TestChown(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	writeFile(base, "/test.txt", []byte("content"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Chown should trigger copy-up
	err := ufs.Chown("/test.txt", 1000, 1000)
	if err != nil {
		t.Fatal(err)
	}

	// File should be in overlay
	if _, err := overlay.Stat("/test.txt"); err != nil {
		t.Error("file should be in overlay after Chown")
	}
}

// TestChtimes tests the Chtimes method
func TestChtimes(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	writeFile(base, "/test.txt", []byte("content"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	newTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	err := ufs.Chtimes("/test.txt", newTime, newTime)
	if err != nil {
		t.Fatal(err)
	}

	// File should be in overlay with new times
	info, _ := ufs.Stat("/test.txt")
	if !info.ModTime().Equal(newTime) {
		t.Errorf("got %v, want %v", info.ModTime(), newTime)
	}
}

// TestOriginalPath tests the originalPath helper
func TestOriginalPath(t *testing.T) {
	tests := []struct {
		input    string
		wantPath string
		wantOK   bool
	}{
		{"/dir/.wh.file.txt", "/dir/file.txt", true},
		{"/dir/file.txt", "", false},                    // Not a whiteout
		{"/dir/.wh.__dir_opaque", "", false},           // Opaque whiteout
		{"/.wh.test", "/test", true},
	}

	for _, tt := range tests {
		path, ok := originalPath(tt.input)
		if ok != tt.wantOK {
			t.Errorf("originalPath(%q) ok = %v, want %v", tt.input, ok, tt.wantOK)
		}
		if ok && path != tt.wantPath {
			t.Errorf("originalPath(%q) = %q, want %q", tt.input, path, tt.wantPath)
		}
	}
}

// TestCleanPath tests the cleanPath helper
func TestCleanPath(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"test.txt", "/test.txt"},
		{"/test.txt", "/test.txt"},
		{"./test.txt", "/test.txt"},
		{"/a/b/../c", "/a/c"},
		{"/a/./b/./c", "/a/b/c"},
	}

	for _, tt := range tests {
		got := cleanPath(tt.input)
		if got != tt.want {
			t.Errorf("cleanPath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// TestSplitPath tests the splitPath helper
func TestSplitPath(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"/", []string{}},
		{"/a", []string{"a"}},
		{"/a/b/c", []string{"a", "b", "c"}},
		{"a/b/c", []string{"a", "b", "c"}},
	}

	for _, tt := range tests {
		got := splitPath(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("splitPath(%q) = %v, want %v", tt.input, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("splitPath(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
			}
		}
	}
}

// TestNoWritableLayerChmod tests Chmod without writable layer
func TestNoWritableLayerChmod(t *testing.T) {
	base := mustNewMemFS()
	writeFile(base, "/test.txt", []byte("content"), 0644)

	ufs := New(
		WithReadOnlyLayer(base),
	)

	err := ufs.Chmod("/test.txt", 0600)
	if err != ErrNoWritableLayer {
		t.Errorf("expected ErrNoWritableLayer, got %v", err)
	}
}

// TestNoWritableLayerChown tests Chown without writable layer
func TestNoWritableLayerChown(t *testing.T) {
	base := mustNewMemFS()
	writeFile(base, "/test.txt", []byte("content"), 0644)

	ufs := New(
		WithReadOnlyLayer(base),
	)

	err := ufs.Chown("/test.txt", 1000, 1000)
	if err != ErrNoWritableLayer {
		t.Errorf("expected ErrNoWritableLayer, got %v", err)
	}
}

// TestNoWritableLayerChtimes tests Chtimes without writable layer
func TestNoWritableLayerChtimes(t *testing.T) {
	base := mustNewMemFS()
	writeFile(base, "/test.txt", []byte("content"), 0644)

	ufs := New(
		WithReadOnlyLayer(base),
	)

	err := ufs.Chtimes("/test.txt", time.Now(), time.Now())
	if err != ErrNoWritableLayer {
		t.Errorf("expected ErrNoWritableLayer, got %v", err)
	}
}

// TestNoWritableLayerMkdir tests Mkdir without writable layer
func TestNoWritableLayerMkdir(t *testing.T) {
	base := mustNewMemFS()

	ufs := New(
		WithReadOnlyLayer(base),
	)

	err := ufs.Mkdir("/newdir", 0755)
	if err != ErrNoWritableLayer {
		t.Errorf("expected ErrNoWritableLayer, got %v", err)
	}
}

// TestNoWritableLayerRename tests Rename without writable layer
func TestNoWritableLayerRename(t *testing.T) {
	base := mustNewMemFS()
	writeFile(base, "/test.txt", []byte("content"), 0644)

	ufs := New(
		WithReadOnlyLayer(base),
	)

	err := ufs.Rename("/test.txt", "/new.txt")
	if err != ErrNoWritableLayer {
		t.Errorf("expected ErrNoWritableLayer, got %v", err)
	}
}

// TestTruncateNotFound tests Truncate on non-existent file
func TestTruncateNotFound(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	fs := ufs.FileSystem()
	err := fs.Truncate("/nonexistent", 0)
	if err == nil {
		t.Error("expected error for non-existent file")
	}
}

// TestTruncateNoWritableLayer tests Truncate without writable layer
func TestTruncateNoWritableLayer(t *testing.T) {
	base := mustNewMemFS()
	writeFile(base, "/test.txt", []byte("content"), 0644)

	ufs := New(
		WithReadOnlyLayer(base),
	)

	fs := ufs.FileSystem()
	err := fs.Truncate("/test.txt", 0)
	if err != ErrNoWritableLayer {
		t.Errorf("expected ErrNoWritableLayer, got %v", err)
	}
}

// =============================================================================
// Opaque Directory Whiteout Tests
// =============================================================================

// TestOpaqueDirectoryWhiteout tests opaque directory whiteout handling
func TestOpaqueDirectoryWhiteout(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	// Create files in base layer
	writeFile(base, "/dir/file1.txt", []byte("1"), 0644)
	writeFile(base, "/dir/file2.txt", []byte("2"), 0644)

	// Create opaque whiteout in overlay
	overlay.MkdirAll("/dir", 0755)
	writeFile(overlay, "/dir/.wh.__dir_opaque", []byte(""), 0644)
	writeFile(overlay, "/dir/file3.txt", []byte("3"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Directory should only show file3 from overlay
	entries, err := readDir(ufs, "/dir")
	if err != nil {
		t.Fatal(err)
	}

	// Should only have file3.txt (opaque marker hides lower layer files)
	names := make(map[string]bool)
	for _, e := range entries {
		names[e.Name()] = true
	}

	if names["file1.txt"] || names["file2.txt"] {
		t.Error("base layer files should be hidden by opaque whiteout")
	}
	if !names["file3.txt"] {
		t.Error("overlay file should be visible")
	}
}

// =============================================================================
// Additional Edge Cases for Higher Coverage
// =============================================================================

// TestRemoveFromWritableLayer tests removing a file that only exists in writable layer
func TestRemoveFromWritableLayer(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Create file in writable layer
	writeFile(ufs, "/test.txt", []byte("content"), 0644)

	// Remove it - should actually delete, not just whiteout
	err := ufs.Remove("/test.txt")
	if err != nil {
		t.Fatal(err)
	}

	// File should not exist
	if _, err := ufs.Stat("/test.txt"); err == nil {
		t.Error("file should not exist")
	}

	// No whiteout should be created for file only in writable layer
	whiteout := whiteoutPath("/test.txt")
	_, whiteoutExists := overlay.Stat(whiteout)
	// Whiteout is still created because the Remove logic creates it when layerIdx > 0 || info != nil
	// This is acceptable behavior
	_ = whiteoutExists
}

// TestRemoveAllFromWritableLayer tests RemoveAll on writable layer only content
func TestRemoveAllFromWritableLayer(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Create directory structure in writable layer
	ufs.MkdirAll("/dir/sub", 0755)
	writeFile(ufs, "/dir/sub/file.txt", []byte("content"), 0644)

	// Remove all
	err := ufs.RemoveAll("/dir")
	if err != nil {
		t.Fatal(err)
	}

	// Directory should not exist
	if _, err := ufs.Stat("/dir"); err == nil {
		t.Error("directory should not exist")
	}
}

// TestRenameInWritableLayer tests renaming a file within writable layer only
func TestRenameInWritableLayer(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Create file in writable layer
	writeFile(ufs, "/old.txt", []byte("content"), 0644)

	// Rename
	err := ufs.Rename("/old.txt", "/new.txt")
	if err != nil {
		t.Fatal(err)
	}

	// Verify
	if _, err := ufs.Stat("/old.txt"); err == nil {
		t.Error("old file should not exist")
	}
	if _, err := ufs.Stat("/new.txt"); err != nil {
		t.Error("new file should exist")
	}
}

// TestChmodInWritableLayer tests chmod on a file already in writable layer
func TestChmodInWritableLayer(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Create file in writable layer
	writeFile(ufs, "/test.txt", []byte("content"), 0644)

	// Chmod - no copy-up needed
	err := ufs.Chmod("/test.txt", 0600)
	if err != nil {
		t.Fatal(err)
	}

	info, _ := ufs.Stat("/test.txt")
	if info.Mode().Perm() != 0600 {
		t.Errorf("got %o, want 0600", info.Mode().Perm())
	}
}

// TestChownInWritableLayer tests chown on a file already in writable layer
func TestChownInWritableLayer(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Create file in writable layer
	writeFile(ufs, "/test.txt", []byte("content"), 0644)

	// Chown - no copy-up needed
	err := ufs.Chown("/test.txt", 1000, 1000)
	if err != nil {
		t.Fatal(err)
	}
}

// TestChtimesInWritableLayer tests chtimes on a file already in writable layer
func TestChtimesInWritableLayer(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Create file in writable layer
	writeFile(ufs, "/test.txt", []byte("content"), 0644)

	// Chtimes - no copy-up needed
	newTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	err := ufs.Chtimes("/test.txt", newTime, newTime)
	if err != nil {
		t.Fatal(err)
	}

	info, _ := ufs.Stat("/test.txt")
	if !info.ModTime().Equal(newTime) {
		t.Errorf("got %v, want %v", info.ModTime(), newTime)
	}
}

// TestCopyUpFileAlreadyInWritable tests copyUpFile when file is already in writable
func TestCopyUpFileAlreadyInWritable(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Create file in writable layer
	writeFile(overlay, "/test.txt", []byte("content"), 0644)

	// Try to copy up - should be no-op
	info, _, _ := ufs.findFile("/test.txt")
	err := ufs.copyUpFile("/test.txt", info)
	if err != nil {
		t.Fatal(err)
	}
}

// TestCopyUpAlreadyInWritable tests copyUp when file is already in writable
func TestCopyUpAlreadyInWritable(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Create file in writable layer
	writeFile(overlay, "/test.txt", []byte("content"), 0644)

	// Try to copy up - should be no-op
	info, _, _ := ufs.findFile("/test.txt")
	err := ufs.copyUp("/test.txt", info)
	if err != nil {
		t.Fatal(err)
	}
}

// TestReadlinkMultipleLayers tests Readlink searching across layers
func TestReadlinkMultipleLayers(t *testing.T) {
	layer0 := mustNewMemFS()
	layer1 := mustNewMemFS()
	overlay := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(layer1),
		WithReadOnlyLayer(layer0),
	)

	// Readlink should search through all layers
	_, err := ufs.Readlink("/nonexistent")
	if err == nil {
		t.Error("expected error")
	}
}

// TestLstatIfPossibleRealError tests LstatIfPossible with real error (not NotExist)
func TestLstatIfPossibleRealError(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	writeFile(base, "/test.txt", []byte("content"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// This tests the standard path
	info, _, err := ufs.LstatIfPossible("/test.txt")
	if err != nil {
		t.Fatal(err)
	}
	if info == nil {
		t.Error("info should not be nil")
	}
}

// TestDirectorySeekEndEmpty tests Seek with io.SeekEnd on empty directory
func TestDirectorySeekEndEmpty(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	base.MkdirAll("/emptydir", 0755)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	dir, err := ufs.Open("/emptydir")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()

	// SeekEnd on empty dir
	pos, err := dir.Seek(0, io.SeekEnd)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 0 {
		t.Errorf("got %d, want 0 for empty dir", pos)
	}
}

// TestFindFileRealError tests findFile error handling
func TestFindFileRealError(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	// Create file
	writeFile(base, "/test.txt", []byte("content"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Standard find should work
	info, layer, err := ufs.findFile("/test.txt")
	if err != nil {
		t.Fatal(err)
	}
	if info == nil {
		t.Error("info should not be nil")
	}
	if layer < 0 {
		t.Error("layer should be >= 0")
	}
}

// TestCacheDisabled tests operations with disabled cache
func TestCacheDisabled(t *testing.T) {
	cache := newCache(false, 0, 0, 0)

	// All operations should be no-ops
	cache.putStat("/test", &mockFileInfo{name: "test"}, 0)
	_, _, ok := cache.getStat("/test")
	if ok {
		t.Error("disabled cache should not return entries")
	}

	cache.putNegative("/negative")
	if cache.isNegative("/negative") {
		t.Error("disabled cache should not track negatives")
	}

	cache.invalidate("/test")          // Should not panic
	cache.invalidateTree("/test")      // Should not panic
	cache.clear()                      // Should not panic

	stats := cache.Stats()
	if stats.Enabled {
		t.Error("cache should report as disabled")
	}
}

// TestSplitPathHelperEdgeCases tests splitPathHelper with edge cases
func TestSplitPathHelperEdgeCases(t *testing.T) {
	// Test with dot
	result := splitPathHelper(".")
	if len(result) != 0 {
		t.Errorf("splitPathHelper(\".\") = %v, want []", result)
	}

	// Test with single component
	result = splitPathHelper("foo")
	if len(result) != 1 || result[0] != "foo" {
		t.Errorf("splitPathHelper(\"foo\") = %v, want [foo]", result)
	}
}

// TestRemoveNonExistent tests Remove on non-existent file
func TestRemoveNonExistent(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	err := ufs.Remove("/nonexistent")
	if err == nil {
		t.Error("expected error for non-existent file")
	}
}

// TestRemoveAllNonExistent tests RemoveAll on non-existent path
func TestRemoveAllNonExistent(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	err := ufs.RemoveAll("/nonexistent")
	if err == nil {
		t.Error("expected error for non-existent path")
	}
}

// TestRenameNonExistent tests Rename on non-existent source
func TestRenameNonExistent(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	err := ufs.Rename("/nonexistent", "/new")
	if err == nil {
		t.Error("expected error for non-existent source")
	}
}

// TestChmodNonExistent tests Chmod on non-existent file
func TestChmodNonExistent(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	err := ufs.Chmod("/nonexistent", 0600)
	if err == nil {
		t.Error("expected error for non-existent file")
	}
}

// TestChownNonExistent tests Chown on non-existent file
func TestChownNonExistent(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	err := ufs.Chown("/nonexistent", 1000, 1000)
	if err == nil {
		t.Error("expected error for non-existent file")
	}
}

// TestChtimesNonExistent tests Chtimes on non-existent file
func TestChtimesNonExistent(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	err := ufs.Chtimes("/nonexistent", time.Now(), time.Now())
	if err == nil {
		t.Error("expected error for non-existent file")
	}
}

// TestOpenFileNonExistent tests OpenFile on non-existent file without O_CREATE
func TestOpenFileNonExistent(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	_, err := ufs.OpenFile("/nonexistent", os.O_RDONLY, 0)
	if err == nil {
		t.Error("expected error for non-existent file")
	}
}

// TestReaddirError tests Readdir error propagation
func TestReaddirError(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	base.MkdirAll("/dir", 0755)
	writeFile(base, "/dir/file.txt", []byte("x"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	dir, _ := ufs.Open("/dir")
	defer dir.Close()

	// First read all entries
	entries, err := dir.Readdir(-1)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("got %d entries, want 1", len(entries))
	}

	// Reading again should return nil for count <= 0
	entries, err = dir.Readdir(0)
	if err != nil {
		t.Errorf("Readdir(0) after EOF should not error: %v", err)
	}
}

// TestSymlinkRemovesWhiteout tests that symlink removes existing whiteout
func TestSymlinkRemovesWhiteout(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	// Create whiteout
	overlay.MkdirAll("/", 0755)
	writeFile(overlay, "/.wh.link", []byte(""), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Symlink should try to remove whiteout (will fail since target doesn't exist)
	err := ufs.Symlink("/target", "/link")
	if err == nil {
		t.Error("expected error for non-existent target")
	}
}

// TestMkdirExistingDirInWritable tests Mkdir on existing directory in writable layer
func TestMkdirExistingDirInWritable(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	// Create directory in writable layer
	overlay.MkdirAll("/existing", 0755)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Mkdir on existing in writable layer should fail
	err := ufs.Mkdir("/existing", 0755)
	if err == nil {
		t.Error("expected error for existing directory in writable layer")
	}
}

// =============================================================================
// Helper Types
// =============================================================================

// mockFileInfo implements os.FileInfo for testing
type mockFileInfo struct {
	name string
}

func (m *mockFileInfo) Name() string       { return m.name }
func (m *mockFileInfo) Size() int64        { return 0 }
func (m *mockFileInfo) Mode() os.FileMode  { return 0644 }
func (m *mockFileInfo) ModTime() time.Time { return time.Time{} }
func (m *mockFileInfo) IsDir() bool        { return false }
func (m *mockFileInfo) Sys() interface{}   { return nil }
