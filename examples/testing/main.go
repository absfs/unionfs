package main

import (
	"fmt"
	"log"

	"github.com/absfs/unionfs"
	"github.com/spf13/afero"
)

// This example demonstrates using UnionFS for testing with fixtures

func main() {
	// Create immutable test fixtures
	fixtures := afero.NewMemMapFs()
	afero.WriteFile(fixtures, "/testdata/users.json", []byte(`[{"id":1,"name":"Alice"}]`), 0644)
	afero.WriteFile(fixtures, "/testdata/config.json", []byte(`{"env":"test"}`), 0644)

	// Create test-specific overlay
	testOverlay := afero.NewMemMapFs()

	// Create union filesystem
	ufs := unionfs.New(
		unionfs.WithWritableLayer(testOverlay),
		unionfs.WithReadOnlyLayer(fixtures),
	)

	fmt.Println("=== Testing with UnionFS Example ===\n")

	// 1. Read fixture data
	fmt.Println("1. Reading from fixtures:")
	data, err := afero.ReadFile(ufs, "/testdata/users.json")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("   users.json: %s\n\n", string(data))

	// 2. Modify data for test (doesn't affect fixtures)
	fmt.Println("2. Modifying data for test:")
	err = afero.WriteFile(ufs, "/testdata/users.json", []byte(`[{"id":2,"name":"Bob"}]`), 0644)
	if err != nil {
		log.Fatal(err)
	}

	data, _ = afero.ReadFile(ufs, "/testdata/users.json")
	fmt.Printf("   Modified view: %s\n", string(data))

	data, _ = afero.ReadFile(fixtures, "/testdata/users.json")
	fmt.Printf("   Original fixtures: %s\n\n", string(data))

	// 3. Add test-specific files
	fmt.Println("3. Adding test-specific files:")
	err = afero.WriteFile(ufs, "/testdata/test-output.txt", []byte("test results"), 0644)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("   Created test-output.txt in overlay\n")

	// 4. List merged directory
	fmt.Println("4. Listing /testdata (merged):")
	entries, err := afero.ReadDir(ufs, "/testdata")
	if err != nil {
		log.Fatal(err)
	}
	for _, entry := range entries {
		fmt.Printf("   - %s\n", entry.Name())
	}
	fmt.Println()

	// 5. Simulate test cleanup by discarding overlay
	fmt.Println("5. After test cleanup (discard overlay):")
	fmt.Println("   Overlay layer is discarded, fixtures remain pristine")

	// Verify fixtures are unchanged
	data, _ = afero.ReadFile(fixtures, "/testdata/users.json")
	fmt.Printf("   Fixtures still have: %s\n", string(data))

	_, err = fixtures.Stat("/testdata/test-output.txt")
	if err != nil {
		fmt.Println("   test-output.txt doesn't exist in fixtures (as expected)")
	}

	fmt.Println("\n=== Example Complete ===")
}
