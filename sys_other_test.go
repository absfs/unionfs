//go:build !unix

package unionfs

import (
	"os"
	"testing"
)

// hostMode reports no host mode bits here.
func hostMode(info os.FileInfo) (uint32, bool) { return 0, false }

// hostSameFile cannot tell here: a host record holds no file ID.
func TestHostSameFileUnknown(t *testing.T) {
	if same, known := hostSameFile(1, 1); same || known {
		t.Errorf("hostSameFile = %v, %v", same, known)
	}
}
