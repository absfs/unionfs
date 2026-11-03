//go:build unix

package unionfs

import (
	"os"
	"syscall"
)

// hostMode returns the mode bits of a host stat record in info's Sys value.
func hostMode(info os.FileInfo) (uint32, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint32(st.Mode), true
}
