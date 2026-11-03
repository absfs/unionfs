package unionfs

import (
	"os"
	"syscall"
	"time"
)

// hostAtime returns the access time the host records for info, and
// whether it could be read.
func hostAtime(info os.FileInfo) (time.Time, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(st.Atimespec.Sec, st.Atimespec.Nsec), true
}

// hostMCtime returns the modification and change times the host records
// for info, and whether they could be read.
func hostMCtime(info os.FileInfo) (time.Time, time.Time, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	return time.Unix(st.Mtimespec.Sec, st.Mtimespec.Nsec), time.Unix(st.Ctimespec.Sec, st.Ctimespec.Nsec), true
}
