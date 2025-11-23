package main

import (
	"fmt"
	"log"

	"github.com/absfs/unionfs"
	"github.com/spf13/afero"
)

func main() {
	// Create base layer (read-only) with some initial files
	baseLayer := afero.NewMemMapFs()
	afero.WriteFile(baseLayer, "/etc/config.yml", []byte("base: config"), 0644)
	afero.WriteFile(baseLayer, "/app/data.txt", []byte("base data"), 0644)

	// Create overlay layer (writable)
	overlayLayer := afero.NewMemMapFs()

	// Create union filesystem
	ufs := unionfs.New(
		unionfs.WithWritableLayer(overlayLayer),
		unionfs.WithReadOnlyLayer(baseLayer),
	)

	fmt.Println("=== Basic UnionFS Example ===\n")

	// 1. Read file from base layer
	fmt.Println("1. Reading file from base layer:")
	data, err := afero.ReadFile(ufs, "/etc/config.yml")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("   /etc/config.yml: %s\n\n", string(data))

	// 2. Write new file to overlay
	fmt.Println("2. Writing new file to overlay:")
	err = afero.WriteFile(ufs, "/etc/custom.yml", []byte("custom: config"), 0644)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("   Created /etc/custom.yml in overlay layer\n")

	// 3. Modify existing file (copy-on-write)
	fmt.Println("3. Modifying file from base layer (triggers CoW):")
	err = afero.WriteFile(ufs, "/etc/config.yml", []byte("modified: config"), 0644)
	if err != nil {
		log.Fatal(err)
	}

	// Read from union
	data, _ = afero.ReadFile(ufs, "/etc/config.yml")
	fmt.Printf("   Union view: %s\n", string(data))

	// Read from base (unchanged)
	data, _ = afero.ReadFile(baseLayer, "/etc/config.yml")
	fmt.Printf("   Base layer: %s\n\n", string(data))

	// 4. Delete file using whiteout
	fmt.Println("4. Deleting file from base layer:")
	err = ufs.Remove("/app/data.txt")
	if err != nil {
		log.Fatal(err)
	}

	// File should not be visible in union
	_, err = ufs.Stat("/app/data.txt")
	if err != nil {
		fmt.Println("   File successfully hidden via whiteout")
	}

	// But still exists in base layer
	_, err = baseLayer.Stat("/app/data.txt")
	if err == nil {
		fmt.Println("   File still exists in base layer")
	}

	fmt.Println("\n=== Example Complete ===")
}
