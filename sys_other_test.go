//go:build !unix

package unionfs

import "os"

// hostMode reports no host mode bits here.
func hostMode(info os.FileInfo) (uint32, bool) { return 0, false }
