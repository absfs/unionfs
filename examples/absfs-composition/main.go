// Package main demonstrates composing UnionFS with other absfs ecosystem packages
// to build complex, layered filesystem behaviors through simple composition.
package main

import (
	"fmt"
	"log"

	"github.com/absfs/memfs"
	"github.com/absfs/unionfs"
)

func main() {
	fmt.Println("=== absfs Ecosystem Composability Demo ===")

	// Demonstrate 1: Basic UnionFS with absfs.FileSystem interface
	basicAbsfsExample()

	fmt.Println()

	// Demonstrate 2: Composing with other absfs packages (conceptual)
	compositionPatternExample()
}

// basicAbsfsExample shows how to use UnionFS as an absfs.FileSystem
func basicAbsfsExample() {
	fmt.Println("1. UnionFS as absfs.FileSystem:")
	fmt.Println("   --------------------------------")

	// Create layers
	baseLayer, err := memfs.NewFS(); if err != nil { log.Fatal(err) }
	overlayLayer, err := memfs.NewFS(); if err != nil { log.Fatal(err) }

	// Set up base layer content
	writeFile(baseLayer, "/etc/app.conf", []byte("base-config"), 0644)
	writeFile(baseLayer, "/usr/bin/app", []byte("#!/bin/bash\necho base"), 0755)

	// Create UnionFS
	ufs := unionfs.New(
		unionfs.WithWritableLayer(overlayLayer),
		unionfs.WithReadOnlyLayer(baseLayer),
	)

	// Get absfs.FileSystem interface
	fs := ufs.FileSystem()

	// Use FileSystem interface methods
	fs.Chdir("/etc")
	cwd, _ := fs.Getwd()
	fmt.Printf("   Current working directory: %s\n", cwd)

	// Read file using relative path (thanks to Chdir/Getwd)
	file, err := fs.Open("app.conf")
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()

	buf := make([]byte, 128)
	n, _ := file.Read(buf)
	fmt.Printf("   Read from base layer: %s\n", buf[:n])

	// Write to overlay (copy-on-write in action)
	fs.Chdir("/etc")
	newFile, err := fs.Create("custom.conf")
	if err != nil {
		log.Fatal(err)
	}
	newFile.Write([]byte("overlay-config"))
	newFile.Close()

	fmt.Println("   ✓ Written to overlay layer")
	fmt.Println("   ✓ absfs.FileSystem interface working!")
}

// compositionPatternExample demonstrates the absfs composition pattern
func compositionPatternExample() {
	fmt.Println("2. absfs Ecosystem Composition Pattern:")
	fmt.Println("   -------------------------------------")

	// Create base UnionFS
	baseLayer, err := memfs.NewFS(); if err != nil { log.Fatal(err) }
	overlayLayer, err := memfs.NewFS(); if err != nil { log.Fatal(err) }

	writeFile(baseLayer, "/data/file.txt", []byte("important data"), 0644)

	ufs := unionfs.New(
		unionfs.WithWritableLayer(overlayLayer),
		unionfs.WithReadOnlyLayer(baseLayer),
	)

	// Get absfs.FileSystem view
	fs := ufs.FileSystem()

	fmt.Println("   Base: UnionFS (multi-layer composition)")
	fmt.Println("   └─ Can be wrapped with:")
	fmt.Println("      • rofs      - Make read-only")
	fmt.Println("      • cachefs   - Add caching layer")
	fmt.Println("      • metricsfs - Add Prometheus metrics")
	fmt.Println("      • retryfs   - Add retry logic")
	fmt.Println("      • permfs    - Add access control")
	fmt.Println()

	fmt.Println("   Example composition (pseudo-code):")
	fmt.Println("      base := unionfs.New(...).FileSystem()")
	fmt.Println("      cached := cachefs.New(base)")
	fmt.Println("      monitored := metricsfs.New(cached)")
	fmt.Println("      protected := rofs.New(monitored)")
	fmt.Println()

	// Demonstrate that UnionFS works with absfs patterns
	testFile, _ := fs.Open("/data/file.txt")
	defer testFile.Close()

	info, _ := testFile.Stat()
	fmt.Printf("   ✓ File access works: %s (%d bytes)\n", info.Name(), info.Size())

	// Show how ExtendFiler adds convenience methods
	fmt.Println()
	fmt.Println("   absfs.ExtendFiler pattern:")
	fmt.Println("   • UnionFS implements Filer (8 core methods)")
	fmt.Println("   • ExtendFiler adds FileSystem methods automatically")
	fmt.Println("   • Result: Full FileSystem interface with minimal code")
	fmt.Println("   ✓ Single responsibility + composability!")
}

// Example showing how you might compose with actual absfs packages
// (This is conceptual - requires actual absfs packages to be available)
func conceptualFullComposition() {
	/*
		// Commented out because it requires external packages
		// This shows the IDEAL composition pattern in the absfs ecosystem

		import (
			"github.com/absfs/memfs"
			"github.com/absfs/osfs"
			"github.com/absfs/rofs"
			"github.com/absfs/cachefs"
			"github.com/absfs/metricsfs"
		)

		// Layer 1: OS filesystem as base
		osBase := osfs.NewFS()

		// Layer 2: In-memory overlay
		memOverlay := memfs.NewFS()

		// Create union of layers
		union := unionfs.New(
			unionfs.WithWritableLayer(memOverlay),
			unionfs.WithReadOnlyLayer(osBase),
		).FileSystem()

		// Wrap with caching for performance
		cached := cachefs.New(union)

		// Add read-only enforcement
		readOnly := rofs.New(cached)

		// Add metrics/observability
		monitored := metricsfs.New(readOnly)

		// Use the fully composed filesystem
		monitored.Chdir("/app")
		file, _ := monitored.Open("config.yml")

		// Every layer adds ONE responsibility:
		// • unionfs: Multi-layer composition + copy-on-write
		// • cachefs: Performance optimization
		// • rofs: Read-only enforcement
		// • metricsfs: Observability
		//
		// Each can be added/removed independently!
	*/
}

func init() {
	// Suppress unused function warning
	_ = conceptualFullComposition
}
