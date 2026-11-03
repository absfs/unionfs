/*
Package unionfs merges a stack of absfs filesystems into one view, with a
writable layer on top of read-only layers: Docker-style overlays, with
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
that starts with WhiteoutPrefix fails with EPERM.

# Writing

Every change goes to the writable layer:

  - Opening an existing lower file for writing, truncating it, or changing
    its mode, owner or times first copies it up: contents, mode and
    modification time. Symbolic links are copied up as links. The
    directories above it are copied up as empty directories with their
    modes and times.
  - A new file, directory or link is created in the writable layer, and a
    whiteout for its name, if any, is removed.
  - Remove and RemoveAll delete the writable layer's copy and write a
    whiteout only when a lower layer still has the name. A directory can be
    removed when its merged view is empty; whiteouts inside its upper copy
    go with it. RemoveAll of a lower tree writes one whiteout for its root.
  - Mkdir over a whited-out directory makes the new directory opaque, so
    none of the old lower entries reappear under it.
  - Rename follows rename(2): it replaces a non-directory, or an empty
    directory when moving a directory, and fails with EISDIR, ENOTDIR,
    ENOTEMPTY or EINVAL as rename(2) does. A file or link only a lower layer
    has is copied up first. A directory that lower layers contribute to is
    copied up whole, its merged tree with no markers left inside, and then
    moved; one only the writable layer has is moved directly. The moved
    directory is made opaque when a lower layer has the new name, and the
    old name is whited out when a lower layer still has it.

The read-only layers are never written to. A union built without
WithWritableLayer is read-only: every change fails with ErrNoWritableLayer.

# Whiteout format

The writable layer records deletions with marker files, in the AUFS
format that Docker's image layers also use:

  - A whiteout is an empty regular file named WhiteoutPrefix + name
    (".wh.name") in the directory that held name. It hides name, and
    everything under it, in every layer below the one that holds it.
  - The opaque marker is an empty regular file named OpaqueWhiteout
    (".wh.__dir_opaque") inside a directory. It hides all of that
    directory's entries in the layers below.

A whiteout and an entry of the same name never coexist in the writable
layer: creating the entry removes the whiteout, and a whiteout is only
written once the entry is gone.

The writable layer can therefore be read on its own as a changeset against
the layers below: a whiteout W records that WhiteoutTarget(W) was deleted,
an opaque marker records that its directory replaced the lower one (the
lower directory's entries are all gone, and its entries in the writable
layer are the whole new contents), and every other entry was created,
modified, or copied up because its metadata changed. IsWhiteout,
IsOpaqueMarker, WhiteoutTarget and WhiteoutName recognise and build marker
names. See the README for a worked example.

Linux overlayfs records the same facts differently (character devices 0/0
for whiteouts, an xattr for opaque directories); unionfs does not read or
write that form.

# Working directory

Relative names are taken from the union's working directory, "/" until
Chdir changes it. FileSystem and SymlinkFileSystem return views of the
union that keep their own working directory.

# Caching

WithStatCache and WithCacheConfig enable a cache of Stat results. Changes
made through the union drop the entries at and under the paths they touch.
Changes made to a layer directly, and changes seen through a symbolic link
to the changed path, show once the entry expires, or after InvalidateCache,
InvalidateCacheTree or ClearCache.

# Concurrency

A UnionFS is safe for concurrent use. A change holds an exclusive lock
while it resolves the path, copies up and writes markers; lookups and
listings share a read lock. Reads and writes through an open file handle do
not take the lock. A file opened for reading from a lower layer keeps
reading the lower copy after another handle copies it up.

# Caveats

  - Copy-up keeps mode and modification time, not ownership; a lower file
    owned by another user is owned by the union's user once copied up.
  - Copy-up runs with the union's own permissions: a lower directory
    without write permission for its owner is copied up with that mode, and
    creating files inside it then fails on a host filesystem, as it would
    without the union.
  - Special files (devices, pipes, sockets) cannot be copied up; changing
    one fails with ENOTSUP.
  - Hard links are not tracked across layers: copying up one name of a
    hard-linked lower file breaks the link.
  - Renaming a large lower directory copies its whole tree.
  - Operations are not atomic against layer failures: a change that fails
    part way (a full disk, say) can leave its copy-up behind, though not a
    half-written file.
*/
package unionfs
