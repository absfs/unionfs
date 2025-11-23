package unionfs

import (
	"fmt"
	"testing"
	"time"

	"github.com/spf13/afero"
)

// BenchmarkStatWithoutCache benchmarks Stat operations without caching
func BenchmarkStatWithoutCache(b *testing.B) {
	baseLayer := afero.NewMemMapFs()
	overlay := afero.NewMemMapFs()

	// Create files in base layer
	for i := 0; i < 100; i++ {
		afero.WriteFile(baseLayer, fmt.Sprintf("/file%d.txt", i), []byte("content"), 0644)
	}

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(baseLayer),
	)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := ufs.Stat("/file50.txt")
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStatWithCache benchmarks Stat operations with caching enabled
func BenchmarkStatWithCache(b *testing.B) {
	baseLayer := afero.NewMemMapFs()
	overlay := afero.NewMemMapFs()

	// Create files in base layer
	for i := 0; i < 100; i++ {
		afero.WriteFile(baseLayer, fmt.Sprintf("/file%d.txt", i), []byte("content"), 0644)
	}

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(baseLayer),
		WithStatCache(true, 5*time.Minute),
	)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := ufs.Stat("/file50.txt")
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkNegativeLookupWithoutCache benchmarks non-existent file lookups without cache
func BenchmarkNegativeLookupWithoutCache(b *testing.B) {
	baseLayer := afero.NewMemMapFs()
	overlay := afero.NewMemMapFs()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(baseLayer),
	)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := ufs.Stat("/nonexistent.txt")
		if err == nil {
			b.Fatal("expected error for nonexistent file")
		}
	}
}

// BenchmarkNegativeLookupWithCache benchmarks non-existent file lookups with cache
func BenchmarkNegativeLookupWithCache(b *testing.B) {
	baseLayer := afero.NewMemMapFs()
	overlay := afero.NewMemMapFs()

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(baseLayer),
		WithStatCache(true, 5*time.Minute),
	)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := ufs.Stat("/nonexistent.txt")
		if err == nil {
			b.Fatal("expected error for nonexistent file")
		}
	}
}

// BenchmarkReadFile benchmarks reading files from base layer
func BenchmarkReadFile(b *testing.B) {
	baseLayer := afero.NewMemMapFs()
	overlay := afero.NewMemMapFs()

	content := make([]byte, 1024) // 1KB
	for i := range content {
		content[i] = byte(i % 256)
	}
	afero.WriteFile(baseLayer, "/test.txt", content, 0644)

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(baseLayer),
	)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := afero.ReadFile(ufs, "/test.txt")
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWriteFile benchmarks writing files to overlay
func BenchmarkWriteFile(b *testing.B) {
	baseLayer := afero.NewMemMapFs()

	content := make([]byte, 1024) // 1KB
	for i := range content {
		content[i] = byte(i % 256)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		overlay := afero.NewMemMapFs()
		ufs := New(
			WithWritableLayer(overlay),
			WithReadOnlyLayer(baseLayer),
		)

		err := afero.WriteFile(ufs, fmt.Sprintf("/test%d.txt", i), content, 0644)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCopyOnWrite benchmarks copy-on-write operations
func BenchmarkCopyOnWrite(b *testing.B) {
	content := make([]byte, 10240) // 10KB
	for i := range content {
		content[i] = byte(i % 256)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		baseLayer := afero.NewMemMapFs()
		overlay := afero.NewMemMapFs()

		afero.WriteFile(baseLayer, "/test.txt", content, 0644)

		ufs := New(
			WithWritableLayer(overlay),
			WithReadOnlyLayer(baseLayer),
		)

		// Trigger copy-on-write
		err := afero.WriteFile(ufs, "/test.txt", []byte("modified"), 0644)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDirectoryMerge benchmarks directory listing with merging
func BenchmarkDirectoryMerge(b *testing.B) {
	layer0 := afero.NewMemMapFs()
	layer1 := afero.NewMemMapFs()
	overlay := afero.NewMemMapFs()

	// Create files in different layers
	for i := 0; i < 50; i++ {
		afero.WriteFile(layer0, fmt.Sprintf("/dir/file%d.txt", i), []byte("0"), 0644)
	}
	for i := 50; i < 100; i++ {
		afero.WriteFile(layer1, fmt.Sprintf("/dir/file%d.txt", i), []byte("1"), 0644)
	}
	for i := 100; i < 150; i++ {
		afero.WriteFile(overlay, fmt.Sprintf("/dir/file%d.txt", i), []byte("2"), 0644)
	}

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(layer1),
		WithReadOnlyLayer(layer0),
	)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entries, err := afero.ReadDir(ufs, "/dir")
		if err != nil {
			b.Fatal(err)
		}
		if len(entries) != 150 {
			b.Fatalf("expected 150 entries, got %d", len(entries))
		}
	}
}

// BenchmarkLayerLookupDepth benchmarks file lookups with varying layer depths
func BenchmarkLayerLookupDepth(b *testing.B) {
	depths := []int{2, 5, 10}

	for _, depth := range depths {
		b.Run(fmt.Sprintf("Layers=%d", depth), func(b *testing.B) {
			// Create layers
			layers := make([]afero.Fs, depth)
			for i := 0; i < depth; i++ {
				layers[i] = afero.NewMemMapFs()
			}

			// Put file in bottom layer
			afero.WriteFile(layers[depth-1], "/test.txt", []byte("content"), 0644)

			// Build union with all layers
			opts := make([]Option, 0, depth+1)
			opts = append(opts, WithWritableLayer(afero.NewMemMapFs()))
			for i := 0; i < depth; i++ {
				opts = append(opts, WithReadOnlyLayer(layers[i]))
			}

			ufs := New(opts...)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := ufs.Stat("/test.txt")
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkMkdirAll benchmarks creating nested directories
func BenchmarkMkdirAll(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		baseLayer := afero.NewMemMapFs()
		overlay := afero.NewMemMapFs()

		ufs := New(
			WithWritableLayer(overlay),
			WithReadOnlyLayer(baseLayer),
		)

		err := ufs.MkdirAll("/a/b/c/d/e/f", 0755)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWhiteoutLookup benchmarks file lookup with whiteouts
func BenchmarkWhiteoutLookup(b *testing.B) {
	baseLayer := afero.NewMemMapFs()
	overlay := afero.NewMemMapFs()

	// Create files in base
	for i := 0; i < 100; i++ {
		afero.WriteFile(baseLayer, fmt.Sprintf("/file%d.txt", i), []byte("content"), 0644)
	}

	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(baseLayer),
	)

	// Delete half the files (create whiteouts)
	for i := 0; i < 50; i++ {
		ufs.Remove(fmt.Sprintf("/file%d.txt", i))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Try to stat a whited-out file
		_, err := ufs.Stat("/file25.txt")
		if err == nil {
			b.Fatal("expected file to be whited out")
		}
	}
}
