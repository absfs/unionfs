package main

import (
	"log"
	"os"

	"github.com/absfs/absfs"
)

// writeFile is a helper to write files
func writeFile(fs absfs.FileSystem, name string, data []byte, perm os.FileMode) error {
	f, err := fs.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

// must stops the example on an error.
func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
