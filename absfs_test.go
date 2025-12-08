package unionfs

import (
	"os"
	"testing"

	"github.com/absfs/absfs"
)

// TestAbsFSInterface verifies UnionFS can provide absfs.FileSystem
func TestAbsFSInterface(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Get absfs.FileSystem interface
	fs := ufs.FileSystem()

	// Compile-time check that result implements absfs.FileSystem
	var _ absfs.FileSystem = fs

	t.Log("✓ UnionFS.FileSystem() provides absfs.FileSystem interface")
}

// TestFileSystem verifies FileSystem() returns working absfs.FileSystem
func TestFileSystem(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	// Setup base layer
	writeFile(base, "/etc/config.yml", []byte("base: config"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Get absfs.FileSystem view
	fs := ufs.FileSystem()

	// Verify it's a FileSystem
	var _ absfs.FileSystem = fs

	// Test Chdir/Getwd
	err := fs.Chdir("/etc")
	if err != nil {
		t.Fatalf("Chdir failed: %v", err)
	}

	cwd, err := fs.Getwd()
	if err != nil {
		t.Fatalf("Getwd failed: %v", err)
	}

	// Virtual paths always use forward slashes
	if cwd != "/etc" {
		t.Errorf("Expected cwd=/etc, got %s", cwd)
	}

	// Test Open with relative path (using cwd)
	file, err := fs.Open("config.yml")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer file.Close()

	// Verify file content
	buf := make([]byte, 128)
	n, _ := file.Read(buf)
	content := string(buf[:n])

	if content != "base: config" {
		t.Errorf("Expected 'base: config', got '%s'", content)
	}

	t.Log("✓ FileSystem() provides working absfs.FileSystem")
}

// TestSeparators tests Separator and ListSeparator methods
func TestSeparators(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Get FileSystem interface to access Separator methods
	fs := ufs.FileSystem()

	// Type assert to get access to underlying adapter
	type separatorProvider interface {
		Separator() uint8
		ListSeparator() uint8
	}

	sp, ok := fs.(separatorProvider)
	if !ok {
		t.Fatal("FileSystem doesn't provide Separator methods")
	}

	sep := sp.Separator()
	listSep := sp.ListSeparator()

	if sep != os.PathSeparator {
		t.Errorf("Separator() = %c, want %c", sep, os.PathSeparator)
	}

	if listSep != os.PathListSeparator {
		t.Errorf("ListSeparator() = %c, want %c", listSep, os.PathListSeparator)
	}

	t.Logf("✓ Separator=%c, ListSeparator=%c", sep, listSep)
}

// TestTruncate tests the Truncate method via FileSystem interface
func TestTruncate(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Create a file with content
	err := writeFile(ufs, "/test.txt", []byte("Hello, World!"), 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// Verify initial size
	info, _ := ufs.Stat("/test.txt")
	if info.Size() != 13 {
		t.Errorf("Initial size = %d, want 13", info.Size())
	}

	// Get FileSystem interface to access Truncate
	fs := ufs.FileSystem()

	// Truncate to smaller size
	err = fs.Truncate("/test.txt", 5)
	if err != nil {
		t.Fatalf("Truncate failed: %v", err)
	}

	// Verify truncated size
	info, _ = ufs.Stat("/test.txt")
	if info.Size() != 5 {
		t.Errorf("Truncated size = %d, want 5", info.Size())
	}

	// Verify truncated content
	content, _ := readFile(ufs, "/test.txt")
	if string(content) != "Hello" {
		t.Errorf("Truncated content = '%s', want 'Hello'", content)
	}

	t.Log("✓ Truncate works correctly")
}

// TestTruncateWithCopyOnWrite tests truncate triggers copy-on-write
func TestTruncateWithCopyOnWrite(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	// Create file in base layer
	writeFile(base, "/base.txt", []byte("Base layer content"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Get FileSystem interface
	fs := ufs.FileSystem()

	// Truncate should trigger copy-on-write
	err := fs.Truncate("/base.txt", 4)
	if err != nil {
		t.Fatalf("Truncate with CoW failed: %v", err)
	}

	// Verify file is now in overlay
	info, err := overlay.Stat("/base.txt")
	if err != nil {
		t.Fatalf("File not in overlay after truncate: %v", err)
	}

	if info.Size() != 4 {
		t.Errorf("Truncated size = %d, want 4", info.Size())
	}

	// Verify base layer unchanged
	baseInfo, _ := base.Stat("/base.txt")
	if baseInfo.Size() != 18 {
		t.Errorf("Base layer modified! Size = %d, want 18", baseInfo.Size())
	}

	t.Log("✓ Truncate triggers copy-on-write correctly")
}

// TestAbsFSComposability demonstrates composing with absfs patterns
func TestAbsFSComposability(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	// Setup test data
	writeFile(base, "/app/config.yml", []byte("app: settings"), 0644)
	writeFile(base, "/app/data.json", []byte("{}"), 0644)

	// Create UnionFS
	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Get FileSystem interface
	fs := ufs.FileSystem()

	// Change to /app directory
	fs.Chdir("/app")

	// Create a new file (should go to overlay)
	file, err := fs.Create("custom.yml")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	file.Write([]byte("custom: config"))
	file.Close()

	// Verify file exists
	info, err := fs.Stat("custom.yml")
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}

	if info.Size() != 14 {
		t.Errorf("File size = %d, want 14", info.Size())
	}

	// Verify it's in the overlay layer
	_, err = overlay.Stat("/app/custom.yml")
	if err != nil {
		t.Errorf("File not in overlay: %v", err)
	}

	// Verify base layer untouched
	_, err = base.Stat("/app/custom.yml")
	if err == nil {
		t.Error("File should not be in base layer")
	}

	t.Log("✓ absfs composability working correctly")
}

// TestExtendFilerPattern verifies ExtendFiler provides additional methods
func TestExtendFilerPattern(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Create test directories
	ufs.MkdirAll("/tmp", 0755)
	ufs.MkdirAll("/etc", 0755)

	// FileSystem() uses ExtendFiler internally
	fs := ufs.FileSystem()
	t.Logf("✓ FileSystem() uses ExtendFiler: %T", fs)

	// Verify initial working directory
	cwd, _ := fs.Getwd()
	t.Logf("  Initial cwd: %s", cwd)

	// Verify FileSystem methods are available
	err := fs.Chdir("/tmp")
	if err != nil {
		t.Errorf("Chdir failed: %v", err)
	}

	cwd, err = fs.Getwd()
	if err != nil {
		t.Errorf("Getwd failed: %v", err)
	}

	if cwd != "/tmp" {
		t.Errorf("cwd = %s, want /tmp", cwd)
	}

	// Each FileSystem() call creates a new instance with own state
	fs2 := ufs.FileSystem()
	err = fs2.Chdir("/etc")
	if err != nil {
		t.Errorf("Second Chdir failed: %v", err)
	}

	cwd2, _ := fs2.Getwd()

	if cwd2 != "/etc" {
		t.Errorf("FileSystem() cwd = %s, want /etc", cwd2)
	}

	// Verify fs and fs2 are independent
	cwd1Again, _ := fs.Getwd()
	if cwd1Again != "/tmp" {
		t.Errorf("First fs cwd changed! Got %s, want /tmp", cwd1Again)
	}

	t.Log("✓ ExtendFiler pattern works correctly")
}

// TestTruncateDirectory verifies truncate fails on directories
func TestTruncateDirectory(t *testing.T) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Get FileSystem interface
	fs := ufs.FileSystem()

	// Create a directory
	ufs.Mkdir("/testdir", 0755)

	// Truncate should fail on directory
	err := fs.Truncate("/testdir", 0)
	if err == nil {
		t.Error("Truncate on directory should fail")
	}

	// Check for path error
	if _, ok := err.(*os.PathError); !ok {
		t.Errorf("Expected PathError, got: %T: %v", err, err)
	}

	t.Log("✓ Truncate correctly rejects directories")
}

// BenchmarkFileSystemVsDirectAccess compares absfs.FileSystem vs direct access
func BenchmarkFileSystemVsDirectAccess(b *testing.B) {
	overlay := mustNewMemFS()
	base := mustNewMemFS()

	writeFile(base, "/bench/file.txt", []byte("benchmark data"), 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	b.Run("DirectAccess", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, err := ufs.Stat("/bench/file.txt")
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("FileSystemAccess", func(b *testing.B) {
		fs := ufs.FileSystem()
		fs.Chdir("/bench")

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, err := fs.Stat("file.txt")
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}
