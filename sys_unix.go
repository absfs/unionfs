//go:build unix

package unionfs

import "syscall"

// hostOwner returns the owner in a host stat record.
func hostOwner(sys any) (uid, gid int, ok bool) {
	st, isStat := sys.(*syscall.Stat_t)
	if !isStat || st == nil {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}

// mergeHostSys returns a copy of the host stat record file with the owner and
// permission bits of the record meta. It returns ok false unless both are
// host stat records.
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
	return &out, true
}
