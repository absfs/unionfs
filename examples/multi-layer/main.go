package main

import (
	"fmt"
	"log"

	"github.com/absfs/unionfs"
	"github.com/spf13/afero"
)

func main() {
	// Simulate a Docker-style layered filesystem
	// Layer 0: Base OS
	baseOS := afero.NewMemMapFs()
	afero.WriteFile(baseOS, "/usr/bin/bash", []byte("bash binary"), 0755)
	afero.WriteFile(baseOS, "/usr/lib/libc.so", []byte("libc library"), 0644)

	// Layer 1: Application dependencies
	appDeps := afero.NewMemMapFs()
	afero.WriteFile(appDeps, "/usr/lib/libapp.so", []byte("app library"), 0644)
	afero.WriteFile(appDeps, "/app/README.md", []byte("App docs"), 0644)

	// Layer 2: Application code
	appCode := afero.NewMemMapFs()
	afero.WriteFile(appCode, "/app/main", []byte("app binary"), 0755)
	afero.WriteFile(appCode, "/app/config/defaults.yml", []byte("defaults"), 0644)

	// Layer 3: Runtime modifications (writable)
	runtime := afero.NewMemMapFs()

	// Create union filesystem
	ufs := unionfs.New(
		unionfs.WithWritableLayer(runtime),
		unionfs.WithReadOnlyLayer(appCode),
		unionfs.WithReadOnlyLayer(appDeps),
		unionfs.WithReadOnlyLayer(baseOS),
	)

	fmt.Println("=== Multi-Layer UnionFS Example ===\n")

	// 1. Read file from bottom layer
	fmt.Println("1. Reading from base OS layer:")
	data, err := afero.ReadFile(ufs, "/usr/bin/bash")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("   /usr/bin/bash: %s\n\n", string(data))

	// 2. Read file from middle layer
	fmt.Println("2. Reading from app dependencies layer:")
	data, err = afero.ReadFile(ufs, "/usr/lib/libapp.so")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("   /usr/lib/libapp.so: %s\n\n", string(data))

	// 3. List files from multiple layers
	fmt.Println("3. Listing /usr/lib (merged from multiple layers):")
	entries, err := afero.ReadDir(ufs, "/usr/lib")
	if err != nil {
		log.Fatal(err)
	}
	for _, entry := range entries {
		fmt.Printf("   - %s\n", entry.Name())
	}
	fmt.Println()

	// 4. Add runtime configuration
	fmt.Println("4. Adding runtime configuration:")
	err = afero.WriteFile(ufs, "/app/config/runtime.yml", []byte("runtime: config"), 0644)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("   Created /app/config/runtime.yml\n")

	// 5. Override default configuration
	fmt.Println("5. Overriding default configuration:")
	err = afero.WriteFile(ufs, "/app/config/defaults.yml", []byte("overridden: config"), 0644)
	if err != nil {
		log.Fatal(err)
	}

	data, _ = afero.ReadFile(ufs, "/app/config/defaults.yml")
	fmt.Printf("   Union view: %s\n", string(data))

	data, _ = afero.ReadFile(appCode, "/app/config/defaults.yml")
	fmt.Printf("   Original in app layer: %s\n\n", string(data))

	// 6. List merged directory
	fmt.Println("6. Listing /app/config (merged):")
	entries, err = afero.ReadDir(ufs, "/app/config")
	if err != nil {
		log.Fatal(err)
	}
	for _, entry := range entries {
		fmt.Printf("   - %s\n", entry.Name())
	}

	fmt.Println("\n=== Example Complete ===")
}
