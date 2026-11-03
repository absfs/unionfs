package unionfs

import "os"

// findFile resolves p in the merged view, following symbolic links, and
// reports the layer it was found in.
func (ufs *UnionFS) findFile(p string) (os.FileInfo, int, error) {
	ufs.mu.RLock()
	defer ufs.mu.RUnlock()
	n, err := ufs.resolve(cleanPath(p), true)
	if err != nil {
		return nil, -1, err
	}
	return n.info, n.top(), nil
}

// whiteoutPath and originalPath are the older names of WhiteoutName and
// WhiteoutTarget, kept for the tests written against them.
func whiteoutPath(p string) string { return WhiteoutName(p) }

func originalPath(p string) (string, bool) { return WhiteoutTarget(p) }
