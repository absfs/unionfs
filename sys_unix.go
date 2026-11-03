//go:build unix

package unionfs

import (
	"reflect"
	"syscall"
)

// hostOwner returns the owner in a host stat record.
func hostOwner(sys any) (uid, gid int, ok bool) {
	st, isStat := sys.(*syscall.Stat_t)
	if !isStat || st == nil {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}

// mergeHostSys returns a copy of the host stat record file with the owner,
// permission bits, modification time and change time of the record meta.
// It returns ok false unless both are host stat records.
func mergeHostSys(file, meta any) (any, bool) {
	f, ok := file.(*syscall.Stat_t)
	if !ok || f == nil {
		return nil, false
	}
	m, ok := meta.(*syscall.Stat_t)
	if !ok || m == nil {
		return nil, false
	}
	out := *f
	out.Uid, out.Gid = m.Uid, m.Gid
	out.Mode = f.Mode&^0o7777 | m.Mode&0o7777
	copyHostTimes(&out, m)
	return &out, true
}

// hostTimeFields are the names the Unix ports give the modification and
// change times in a stat record: Mtim and Ctim on Linux, Solaris, OpenBSD
// and others, Mtimespec and Ctimespec on Darwin, FreeBSD and NetBSD.
var hostTimeFields = []string{"Mtim", "Ctim", "Mtimespec", "Ctimespec"}

// copyHostTimes copies the modification and change times of src into dst,
// whichever names the port gives them.
func copyHostTimes(dst, src *syscall.Stat_t) {
	d, s := reflect.ValueOf(dst).Elem(), reflect.ValueOf(src).Elem()
	for _, name := range hostTimeFields {
		if f := d.FieldByName(name); f.IsValid() {
			f.Set(s.FieldByName(name))
		}
	}
}
