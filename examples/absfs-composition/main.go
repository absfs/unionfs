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
	baseLayer, err := memfs.NewFS()
	if err != nil {
		log.Fatal(err)
	}
	overlayLayer, err := memfs.NewFS()
	if err != nil {
		log.Fatal(err)
	}

	// Set up base layer content
	must(baseLayer.MkdirAll("/etc", 0755))
	must(baseLayer.MkdirAll("/usr/bin", 0755))
	must(writeFile(baseLayer, "/etc/app.conf", []byte("base-config"), 0644))
	must(writeFile(baseLayer, "/usr/bin/app", []byte("#!/bin/bash\necho base"), 0755))

	// Create UnionFS
	ufs := unionfs.New(
		unionfs.WithWritableLayer(overlayLayer),
		unionfs.WithReadOnlyLayer(baseLayer),
	)

	// Get absfs.FileSystem interface
	fs := ufs.FileSystem()

	// Use FileSystem interface methods
	must(fs.Chdir("/etc"))
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
	baseLayer, err := memfs.NewFS()
	if err != nil {
		log.Fatal(err)
	}
	overlayLayer, err := memfs.NewFS()
	if err != nil {
		log.Fatal(err)
	}

	must(baseLayer.MkdirAll("/data", 0755))
	must(writeFile(baseLayer, "/data/file.txt", []byte("important data"), 0644))

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
	testFile, err := fs.Open("/data/file.txt")
	if err != nil {
		log.Fatal(err)
	}
	defer testFile.Close()

	info, err := testFile.Stat()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("   ✓ File access works: %s (%d bytes)\n", info.Name(), info.Size())

	// absfs interfaces
	fmt.Println()
	fmt.Println("   absfs interfaces:")
	fmt.Println("   • *UnionFS implements absfs.SymlinkFileSystem directly")
	fmt.Println("   • FileSystem() gives a view with its own working directory")
	fmt.Println("   ✓ Single responsibility + composability!")
}

// The same pattern extends further: the union is an absfs.FileSystem, so
// other absfs wrappers (read-only enforcement, caching, metrics) can be
// layered over it, each adding one responsibility.
