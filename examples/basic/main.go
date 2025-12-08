package main

import (
	"fmt"
	"io"
	"log"
	"os"

	"github.com/absfs/memfs"
	"github.com/absfs/unionfs"
)

// writeFile is a helper to write files
func writeFile(fs interface{ OpenFile(string, int, os.FileMode) (interface{ Write([]byte) (int, error); Close() error }, error) }, name string, data []byte, perm os.FileMode) error {
	f, err := fs.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

// readFile is a helper to read files
func readFile(fs interface{ Open(string) (interface{ Read([]byte) (int, error); Close() error }, error) }, name string) ([]byte, error) {
	f, err := fs.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func main() {
	// Create base layer (read-only) with some initial files
	baseLayer, err := memfs.NewFS(); if err != nil { log.Fatal(err) }
	writeFile(baseLayer, "/etc/config.yml", []byte("base: config"), 0644)
	writeFile(baseLayer, "/app/data.txt", []byte("base data"), 0644)

	// Create overlay layer (writable)
	overlayLayer, err := memfs.NewFS(); if err != nil { log.Fatal(err) }

	// Create union filesystem
	ufs := unionfs.New(
		unionfs.WithWritableLayer(overlayLayer),
		unionfs.WithReadOnlyLayer(baseLayer),
	)

	fmt.Println("=== Basic UnionFS Example ===\n")

	// 1. Read file from base layer
	fmt.Println("1. Reading file from base layer:")
	data, err := readFile(ufs, "/etc/config.yml")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("   /etc/config.yml: %s\n\n", string(data))

	// 2. Write new file to overlay
	fmt.Println("2. Writing new file to overlay:")
	err = writeFile(ufs, "/etc/custom.yml", []byte("custom: config"), 0644)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("   Created /etc/custom.yml in overlay layer\n")

	// 3. Modify existing file (copy-on-write)
	fmt.Println("3. Modifying file from base layer (triggers CoW):")
	err = writeFile(ufs, "/etc/config.yml", []byte("modified: config"), 0644)
	if err != nil {
		log.Fatal(err)
	}

	// Read from union
	data, _ = readFile(ufs, "/etc/config.yml")
	fmt.Printf("   Union view: %s\n", string(data))

	// Read from base (unchanged)
	data, _ = readFile(baseLayer, "/etc/config.yml")
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
