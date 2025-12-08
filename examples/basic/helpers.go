package main

import (
	"io"
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

// readFile is a helper to read files
func readFile(fs absfs.FileSystem, name string) ([]byte, error) {
	f, err := fs.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}
