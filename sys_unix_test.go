//go:build unix

package unionfs

import (
	"os"
	"syscall"
	"testing"
)

// hostMode returns the mode bits of a host stat record in info's Sys value.
func hostMode(info os.FileInfo) (uint32, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint32(st.Mode), true
}

func TestHostSameFileNeedsTwoRecords(t *testing.T) {
	st := &syscall.Stat_t{Dev: 1, Ino: 2}
	for _, other := range []any{nil, (*syscall.Stat_t)(nil), "x"} {
		if same, known := hostSameFile(st, other); same || known {
			t.Errorf("hostSameFile(record, %#v) = %v, %v", other, same, known)
		}
		if same, known := hostSameFile(other, st); same || known {
			t.Errorf("hostSameFile(%#v, record) = %v, %v", other, same, known)
		}
	}
	if same, known := hostSameFile(st, &syscall.Stat_t{Dev: 1, Ino: 2}); !same || !known {
		t.Errorf("equal records: %v, %v", same, known)
	}
	if same, known := hostSameFile(st, &syscall.Stat_t{Dev: 2, Ino: 2}); same || !known {
		t.Errorf("other device: %v, %v", same, known)
	}
}
