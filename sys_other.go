//go:build !unix

package unionfs

// hostOwner reports no owner: host records here carry none the union reads.
func hostOwner(sys any) (uid, gid int, ok bool) { return 0, 0, false }

// mergeHostSys reports that it cannot merge host records here.
func mergeHostSys(file, meta any) (any, bool) { return nil, false }

// hostSameFile cannot tell: a host record here (on Windows, a
// *syscall.Win32FileAttributeData) holds no file ID. Only os.SameFile of
// the os package's own infos, which find it from the file, can.
func hostSameFile(a, b any) (same, known bool) { return false, false }
