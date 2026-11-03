package main

import "testing"

// TestRun runs the example end to end, so that a change to the package
// that breaks it fails go test, and so that its code counts in coverage.
func TestRun(t *testing.T) {
	main()
}
