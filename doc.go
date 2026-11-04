/*
Package unionfs merges a stack of absfs filesystems into one view, with a
writable layer on top of read-only layers: overlayfs-style unions, with
copy-on-write and whiteouts, for any absfs filesystem.

	ufs := unionfs.New(
	    unionfs.WithWritableLayer(upper),  // every change lands here
	    unionfs.WithReadOnlyLayer(lower),  // never modified
	)

*UnionFS implements absfs.SymlinkFileSystem, so a union can be served (for
example over NFS by absnfs), routed (switchfs), or stacked under another
union like any other absfs filesystem.

# Reading

A path is resolved one element at a time. For each element the layers that
contribute to the directory are asked top to bottom: the first layer that
has the name supplies it. A non-directory hides the name in every layer
below. A directory merges with the same directory in the layers below,
down to a layer that holds a whiteout for it, a non-directory of the same
name, or an opaque marker inside it. Symbolic links are resolved against the
merged view, so a link in the writable layer may point at a file only a
lower layer has.

ReadDir and an opened directory list the merged entries, sorted by name,
upper layers shadowing lower ones, whited-out names left out. Whiteout and
opaque marker files never appear in the merged view, and creating a name
that starts with WhiteoutPrefix, in any ASCII case, fails with EPERM (see
Whiteout format for such names in a read-only layer). A host that folds
case takes ".WH.x" for the marker ".wh.x", so the union refuses such
names on every host, and none of them resolves: no spelling of a marker's
name reaches the marker. Where Windows gives names 8.3 short aliases
(".wh.x" is also "WH~1.X"), a name of that shape that a layer whose
markers count resolves, but does not list under that name, fails with
EPERM, whatever entry it aliases.

# Writing

Every change goes to the writable layer:

  - Opening an existing lower file for writing, or truncating it, first
    copies it up: contents, mode and modification time. Changing the
    mode, owner or times of a regular file only lower layers have copies
    nothing: the change goes to a metadata marker beside it in the
    writable layer (see Whiteout format below), and the data is copied
    up only by the first open for writing (even one that writes nothing),
    truncate or rename. Changing the mode, owner or times of a directory
    or link copies it up, which costs no data. Symbolic links are copied
    up as links. The
    directories above it are copied up as empty directories with their
    modes and times, and copying up into a directory leaves its mode and
    modification time as they were. A copy-up below a lower directory its
    owner cannot write (0555) works, as the change would on a host: the
    union makes the copied-up directory writable just while it copies the
    entry in. O_CREATE alone on an existing file is a read and copies
    nothing up, and so is a Chtimes of a lower entry that changes only its
    access time: see UnionFS.Chtimes.
  - A new file, directory or link is created in the writable layer, and a
    whiteout for its name, if any, is removed.
  - Remove and RemoveAll delete the writable layer's copy and write a
    whiteout only when a lower layer still has the name. A directory can be
    removed when its merged view is empty; whiteouts inside its upper copy
    go with it. RemoveAll of a lower tree writes one whiteout for its root.
    The whiteout is written first, and an upper directory is renamed aside
    before it is deleted, so a failure part way never brings a deleted
    lower entry back.
  - Mkdir over a whited-out directory makes the new directory opaque, so
    none of the old lower entries reappear under it.
  - Rename follows rename(2): it replaces a non-directory, or an empty
    directory when moving a directory, and fails with EBUSY, EISDIR,
    ENOTDIR, ENOTEMPTY or EINVAL as rename(2) does. A failed Rename leaves
    its source and its target as they were. A file or link only a lower layer
    has is copied up first. A directory that lower layers contribute to is
    copied up whole and moved, and the markers inside the moved copy are
    then removed; one only the writable layer has is moved directly. The moved
    directory is made opaque when a lower layer has the new name, and the
    old name is whited out when a lower layer still has it.

The read-only layers are never written to. A union built without
WithWritableLayer is read-only: every change fails with ErrNoWritableLayer.

# Whiteout format

The writable layer records deletions with marker files. Whiteouts use the
".wh.name" naming of AUFS and OCI image layers; the opaque marker is this
package's own:

  - A whiteout is an empty regular file named WhiteoutPrefix + name
    (".wh.name") in the directory that held name. It hides name, and
    everything under it, in every layer below the one that holds it.
  - The opaque marker is an empty regular file named OpaqueWhiteout
    (".wh.__dir_opaque") inside a directory. It hides all of that
    directory's entries in the layers below. AUFS and OCI name theirs
    ".wh..wh..opq", which this package does not recognise.
  - Names starting with WhiteoutPrefix twice (".wh..wh.") are reserved for
    metadata, as in AUFS and OCI: they are hidden and delete nothing. The
    union names an entry it is about to delete ".wh..wh.unionfs.<n>"; one
    is left behind only when a layer fails part way through deleting it.
  - A metadata marker is an empty regular file named MetaPrefix + name
    (".wh..wh.meta.name") in the directory that holds name, a regular file
    the layers below have. Its own permission bits (all of them, so the
    execute bits read straight off it), modification time and owner are
    name's; name's contents, size and type are the lower file's. There is
    no entry name in the same layer. Stat, Lstat, ReadDir and a handle
    opened for reading report the marker's metadata, and a FileInfo's Sys
    value carries the marker's owner, mode and modification and change
    times where both layers report one (host directories, memfs). A file
    whose name is longer than 242 bytes is copied up instead, since its
    marker's name would pass the hosts' 255-byte limit. Removing name deletes the marker with it; the
    first copy-up of name gives the copy the marker's metadata and deletes
    the marker. A marker in a read-only layer applies in the same way to
    the layers below it, so a captured upper can be stacked as a lower.

Once an operation has finished, a whiteout and an entry of the same name
never coexist in the writable layer: creating the entry removes the
whiteout, and deleting it leaves only the whiteout. Nor do a metadata
marker and an entry of its name: copying up or replacing the file removes
the marker.

The writable layer can therefore be read on its own as a changeset against
the layers below: a whiteout W records that WhiteoutTarget(W) was deleted,
an opaque marker records that its directory replaced the lower one (the
lower directory's entries are all gone, and its entries in the writable
layer are the whole new contents), a metadata marker M records that only
the mode, times or owner of MetaTarget(M) changed, to M's own, and every
other entry was created, modified, or copied up because a directory's or
link's metadata changed; other reserved ".wh..wh." names are not part of
the changeset. A reader that predates metadata markers skips every
".wh..wh." name, and so sees no change for such a file. IsWhiteout,
IsOpaqueMarker, IsMetaMarker, WhiteoutTarget, WhiteoutName, MetaTarget and
MetaMarkerName recognise and build marker names; they take
slash-separated names without a trailing slash, so convert a host path with
filepath.ToSlash first. See the example for WhiteoutTarget, and the README,
for a walk that does this.

By default markers count in every layer, so that a writable layer captured
from one union can be stacked as a read-only layer of another, as OCI
image layers are. A read-only layer's own entries whose names start with
WhiteoutPrefix are therefore taken for markers: they never appear, and a
whiteout among them hides a name in the layers below. ReservedNames lists
such names in a tree, so a caller can detect a lower the union cannot
represent. WithMarkersOnlyInWritableLayer reads markers only in the
writable layer: in read-only layers such names are ordinary, read-only
entries (changing, removing or renaming one, or creating anything under
one, fails with EPERM), and those layers can carry no deletions or
metadata markers.

Linux overlayfs records the same facts differently (character devices 0/0
for whiteouts, an xattr for opaque directories); unionfs does not read or
write that form.

# Working directory

Relative names are taken from the union's working directory, "/" until
Chdir changes it. FileSystem and SymlinkFileSystem return views of the
union that keep their own working directory.

# Caching

Two caches are off unless an option enables them.

WithLookupCache caches path resolution: which layer holds each name, the
whiteouts, opaque markers and metadata markers that apply to it, and which
names are missing. A lookup served from it reads only the final element's
attributes from its layer, so attributes are never stale, even under
writes through open handles. Every change made through the union drops the
entries it affects. Changes made to a layer directly show once the
entries expire, or after InvalidateCache or InvalidateCacheTree of the
topmost path the change created or removed, or ClearCache; a name removed
from its layer is noticed at once. On a host that takes two spellings for
one name (by case, or by Unicode normalisation on macOS), a change made
through one spelling drops only that spelling's entries, so the other may
show the old view until it expires. A write never makes that old view
permanent: a copy-up, or a new metadata marker, for an entry the cache
resolved first checks it against the layers, and when the merged view has
changed there the cache is emptied and the change fails, with ENOENT when
the name is gone and ESTALE otherwise, so a retry acts on the current
view. The cache never walks through a directory that has since become a
symbolic link: creating a link in the writable layer, or renaming
anything there but a regular file, empties the cache.

WithStatCache and WithCacheConfig enable a cache of Stat and Lstat
results. Changes made through the union drop the entries at and under the
paths they touch, both as the caller named them and as they resolved.
Changes made to a layer directly, writes through a file handle the union
returned (OpenFile drops the entry only when the file is opened), and
changes seen through another symbolic link to the changed path, show once
the entry expires, or after InvalidateCache, InvalidateCacheTree or
ClearCache.

# Concurrency

A UnionFS is safe for concurrent use. A change holds an exclusive lock
while it resolves the path, copies up and writes markers; lookups and
listings share a read lock. Reads and writes through an open file handle do
not take the lock. A file opened for reading from a lower layer keeps
reading the lower copy after another handle copies it up.

# Caveats

  - Copy-up keeps mode and modification time, not ownership; a lower file
    owned by another user is owned by the union's user once copied up. A
    metadata marker is given the lower file's owner, and a copy the
    marker's owner, only where the union's permissions allow that chown;
    otherwise they too are owned by the union's user.
  - Copy-up runs with the union's own permissions: a lower directory
    without write permission for its owner is copied up with that mode, and
    creating, removing or renaming an entry directly inside it then fails
    on a host filesystem, as it would without the union.
  - Special files (devices, pipes, sockets) cannot be copied up; changing
    one fails with ENOTSUP.
  - Hard links are not tracked across layers: copying up one name of a
    hard-linked lower file breaks the link.
  - Renaming a large lower directory copies its whole tree.
  - Operations are not atomic against layer failures: a change that fails
    part way (a full disk, say) can leave its copy-up behind, though not a
    half-written file, and does not bring back deleted entries. A Remove,
    RemoveAll or Rename whose last step, deleting the old upper copy, fails
    has taken effect in the merged view, leaves a hidden ".wh..wh.unionfs.<n>"
    leftover, and reports the error.
  - The caller's path is normalised lexically: a trailing slash is dropped,
    so Stat("/file/") succeeds on a regular file, and an empty name means
    the working directory, except that Remove and Rename fail with ENOENT
    for it and RemoveAll does nothing, as the os functions do. A name ending
    in "." or ".." fails with EINVAL in all three. A symbolic link's target
    is not cleaned: it is walked element by element, so a ".." after a
    linked directory leaves the directory that link resolves to, as on a
    host.
  - The union's own bookkeeping never acts through a symbolic link in the
    writable layer, where a host would follow it: an entry already at a
    whiteout's or opaque marker's path counts as that marker and is not
    opened, a link at a metadata marker's path is not a marker (the change
    fails with EEXIST), copy-up creates its file exclusively, and a
    directory the union expected that is now something else fails with
    ENOTDIR. Lchown of a link fails with errors.ErrUnsupported when the
    writable layer has no Lchown. On a host that folds case, an entry
    named ".WH.x" that is already in the writable layer (the union no
    longer creates one, and does not resolve one) is taken for the marker
    ".wh.x": as a whiteout it hides x from lookups, though ReadDir still
    lists x; as a regular file at a metadata marker's name it becomes x's
    metadata; as a link or directory there it makes metadata changes to x
    fail with EEXIST.
*/
package unionfs
