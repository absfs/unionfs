/*
Package unionfs provides a layered filesystem implementation for Go with Docker-style
overlay capabilities and copy-on-write support.

# Overview

UnionFS enables the composition of multiple filesystem layers into a single unified view.
This is similar to how Docker and other container systems build images through layering,
where each layer can add, modify, or delete files from lower layers.

# Key Features

  - Multiple filesystem layer composition
  - Copy-on-write (CoW) semantics for modifications
  - Whiteout support for deletions across layers
  - Read-only base layers with writable overlay
  - Efficient file lookup through layer precedence
  - Full afero.Fs interface compatibility

# Architecture

The UnionFS maintains a stack of layers, ordered from top (highest precedence) to bottom.
When a file is accessed, layers are searched from top to bottom, and the first match wins.
Write operations always go to the topmost writable layer.

# Basic Usage

	package main

	import (
	    "github.com/absfs/unionfs"
	    "github.com/spf13/afero"
	)

	func main() {
	    // Create base layer (read-only)
	    baseLayer := afero.NewOsFs()

	    // Create overlay layer (writable)
	    overlayLayer := afero.NewMemMapFs()

	    // Create union filesystem
	    ufs := unionfs.New(
	        unionfs.WithWritableLayer(overlayLayer),
	        unionfs.WithReadOnlyLayer(baseLayer),
	    )

	    // Reads fall through to base layer if not in overlay
	    data, err := afero.ReadFile(ufs, "/etc/config.yml")

	    // Writes go to overlay layer
	    err = afero.WriteFile(ufs, "/etc/custom.yml", []byte("key: value"), 0644)

	    // Modifications trigger copy-on-write
	    file, err := ufs.OpenFile("/etc/config.yml", os.O_RDWR, 0)
	    file.Write([]byte("modified")) // Copies to overlay first
	}

# Multiple Layers

You can stack multiple read-only layers with a single writable layer on top:

	ufs := unionfs.New(
	    unionfs.WithWritableLayer(overlayLayer),      // Top: writable
	    unionfs.WithReadOnlyLayer(configLayer),       // Middle: configs
	    unionfs.WithReadOnlyLayer(appLayer),          // Middle: app files
	    unionfs.WithReadOnlyLayer(systemLayer),       // Bottom: system files
	)

Layers are searched in order, so files in higher layers take precedence over lower layers.

# Copy-on-Write

When you modify a file that exists in a read-only lower layer, the file is automatically
copied to the writable layer before modification. This ensures the original file in the
lower layer remains unchanged:

	// File exists in baseLayer
	afero.WriteFile(baseLayer, "/config.txt", []byte("original"), 0644)

	// Create union with overlay
	ufs := unionfs.New(
	    unionfs.WithWritableLayer(overlay),
	    unionfs.WithReadOnlyLayer(baseLayer),
	)

	// Modify file - triggers copy-on-write
	afero.WriteFile(ufs, "/config.txt", []byte("modified"), 0644)

	// Read from union gets modified version
	data, _ := afero.ReadFile(ufs, "/config.txt")  // "modified"

	// Base layer still has original
	data, _ = afero.ReadFile(baseLayer, "/config.txt")  // "original"

# Whiteout Files

When you delete a file that exists in a lower layer, a whiteout marker is created in the
writable layer. This is a special file with the prefix ".wh." that marks the file as deleted:

	// File exists in base layer
	afero.WriteFile(baseLayer, "/file.txt", []byte("content"), 0644)

	// Delete through union
	ufs.Remove("/file.txt")

	// Creates whiteout marker at /.wh.file.txt in overlay
	// File no longer visible through union
	_, err := ufs.Stat("/file.txt")  // os.ErrNotExist

	// But still exists in base layer
	_, err = baseLayer.Stat("/file.txt")  // nil error

Whiteout files follow the AUFS/Docker convention using the ".wh." prefix.

# Directory Merging

When reading a directory, UnionFS merges the contents from all layers:

	// Layer 0 has /dir/file1.txt
	// Layer 1 has /dir/file2.txt
	// Layer 2 has /dir/file3.txt

	entries, _ := afero.ReadDir(ufs, "/dir")
	// Returns all three files: file1.txt, file2.txt, file3.txt

Whiteouts are respected during directory merging, so deleted files don't appear in listings.

# Use Cases

Configuration Management:

	// Base configuration + environment-specific overrides
	ufs := unionfs.New(
	    unionfs.WithWritableLayer(runtimeConfig),
	    unionfs.WithReadOnlyLayer(envConfig),
	    unionfs.WithReadOnlyLayer(defaultConfig),
	)

Testing with Fixtures:

	// Immutable test fixtures + test-specific modifications
	ufs := unionfs.New(
	    unionfs.WithWritableLayer(testOverlay),
	    unionfs.WithReadOnlyLayer(fixtures),
	)

Container Filesystems:

	// Layer base OS, dependencies, app code, and runtime changes
	ufs := unionfs.New(
	    unionfs.WithWritableLayer(runtime),
	    unionfs.WithReadOnlyLayer(appCode),
	    unionfs.WithReadOnlyLayer(dependencies),
	    unionfs.WithReadOnlyLayer(baseOS),
	)

# Performance Considerations

  - File lookups traverse layers from top to bottom, so fewer layers = better performance
  - Copy-on-write operations involve copying the entire file, which can be expensive for large files
  - Directory merging may be slower for directories with many files across multiple layers
  - Consider using stat caching for frequently accessed files (see WithStatCache option)

# Thread Safety

UnionFS uses read-write locks to ensure thread-safe access to the layer stack. Multiple
goroutines can safely read from the filesystem concurrently, while write operations are
properly synchronized.

# Compatibility

UnionFS implements the afero.Fs interface and can be used as a drop-in replacement
wherever afero filesystems are accepted.

# Limitations

  - Only one writable layer is supported (topmost layer)
  - Hard links are not supported across layers
  - Symlink resolution is currently basic (advanced cross-layer symlinks not yet implemented)
  - File locking behavior across layers is filesystem-dependent
*/
package unionfs
