module github.com/absfs/unionfs

go 1.25.4

require (
	github.com/absfs/absfs v0.0.0-20251208163131-5313f0098c48
	github.com/absfs/memfs v0.0.0-20251208165439-f0e964930087
)

require github.com/absfs/inode v0.0.2-0.20251124215006-bac3fa8943ab // indirect

replace (
	github.com/absfs/inode => ../inode
	github.com/absfs/memfs => ../memfs
)
