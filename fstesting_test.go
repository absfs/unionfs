package unionfs

import (
	"testing"

	"github.com/absfs/fstesting"
	"github.com/absfs/memfs"
)

// TestUnionFSSuite runs the fstesting suite against UnionFS with symlink support.
// This tests that UnionFS correctly implements the SymlinkFileSystem interface
// and properly handles symlinks across the writable layer.
func TestUnionFSSuite(t *testing.T) {
	// Create writable layer (overlay)
	overlay, err := memfs.NewFS()
	if err != nil {
		t.Fatalf("failed to create overlay filesystem: %v", err)
	}

	// Create read-only base layer
	base, err := memfs.NewFS()
	if err != nil {
		t.Fatalf("failed to create base filesystem: %v", err)
	}

	// Create UnionFS
	ufs := New(
		WithWritableLayer(overlay),
		WithReadOnlyLayer(base),
	)

	// Get SymlinkFileSystem adapter
	sfs := ufs.SymlinkFileSystem()

	suite := &fstesting.Suite{
		FS: sfs,
		Features: fstesting.Features{
			Symlinks:      true,
			HardLinks:     false, // memfs doesn't support hard links
			Permissions:   true,
			Timestamps:    true,
			CaseSensitive: true,
			AtomicRename:  true,
			SparseFiles:   false,
			LargeFiles:    true,
		},
	}

	suite.Run(t)
}
