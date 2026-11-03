//go:build !unix

package unionfs

// hostOwner reports no owner: host records here carry none the union reads.
func hostOwner(sys any) (uid, gid int, ok bool) { return 0, 0, false }

// mergeHostSys reports that it cannot merge host records here.
func mergeHostSys(file, meta any) (any, bool) { return nil, false }
