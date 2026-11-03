//go:build !darwin && !linux

package unionfs

import (
	"os"
	"time"
)

// hostAtime reports that the access time cannot be read on this platform.
func hostAtime(os.FileInfo) (time.Time, bool) {
	return time.Time{}, false
}

// hostMCtime reports that the host times cannot be read on this platform.
func hostMCtime(os.FileInfo) (time.Time, time.Time, bool) {
	return time.Time{}, time.Time{}, false
}
