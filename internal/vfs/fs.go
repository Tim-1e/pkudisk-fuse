//go:build linux || darwin

package vfs

import (
	"context"
	"errors"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/thezzisu/pkudisk-fuse/internal/anyshare"
	"hash/fnv"
	"os"
	"syscall"
	"time"
)

type inodeBase = fs.Inode

func stable(e anyshare.Entry) fs.StableAttr {
	h := fnv.New64a()
	h.Write([]byte(e.ID))
	ino := h.Sum64()
	if ino < 2 {
		ino += 2
	}
	mode := uint32(syscall.S_IFREG)
	if e.Dir {
		mode = syscall.S_IFDIR
	}
	return fs.StableAttr{Ino: ino, Mode: mode}
}

func errno(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	switch {
	case errors.Is(err, anyshare.ErrNotFound):
		return syscall.ENOENT
	case errors.Is(err, anyshare.ErrExists):
		return syscall.EEXIST
	case errors.Is(err, anyshare.ErrPermission):
		return syscall.EACCES
	case errors.Is(err, anyshare.ErrConflict):
		return syscall.EBUSY
	case errors.Is(err, anyshare.ErrQuota):
		return syscall.ENOSPC
	case errors.Is(err, anyshare.ErrSizeLimit):
		return syscall.EFBIG
	case errors.Is(err, anyshare.ErrPending):
		return syscall.EACCES
	case errors.Is(err, context.Canceled):
		return syscall.EINTR
	case errors.Is(err, context.DeadlineExceeded):
		return syscall.ETIMEDOUT
	}
	var se syscall.Errno
	if errors.As(err, &se) {
		return se
	}
	return syscall.EIO
}
func (n *Node) failure(operation string, err error) syscall.Errno {
	if err != nil {
		n.f.opts.Logger.Printf("%s: %v", operation, err)
	}
	return errno(err)
}

func (n *Node) fillLocked(a *fuse.Attr) {
	a.Mode = n.mode | syscall.S_IFREG
	if n.entry.Dir {
		a.Mode = n.mode | syscall.S_IFDIR
	}
	if n.synthetic {
		a.Mode = syscall.S_IFDIR | 0500
	}
	if n.f.opts.ReadOnly {
		a.Mode &^= 0222
	}
	a.Uid, a.Gid = uint32(os.Getuid()), uint32(os.Getgid())
	a.Nlink = 1
	if n.entry.Dir {
		a.Nlink = 2
	}
	if !n.entry.Dir && n.entry.Size >= 0 {
		a.Size = uint64(n.entry.Size)
	}
	a.Blksize = 4096
	a.Blocks = (a.Size + 511) / 512
	mtime := n.entry.Modified
	if mtime.IsZero() {
		mtime = time.Unix(0, 0)
	}
	created := n.entry.Created
	if created.IsZero() {
		created = mtime
	}
	a.SetTimes(&mtime, &mtime, &created)
}

func (n *Node) Getattr(ctx context.Context, h fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.synthetic && !n.dirty && !n.unlinked && n.refs == 0 && time.Since(n.statAt) >= n.f.opts.TTL {
		entry, err := n.f.backend.Stat(ctx, n.entry)
		if err != nil {
			return n.failure("stat", err)
		}
		n.entry = entry
		n.statAt = time.Now()
	}
	n.fillLocked(&out.Attr)
	return 0
}

func (n *Node) child(ctx context.Context, e anyshare.Entry) *fs.Inode {
	if old := n.GetChild(e.Name); old != nil {
		if child, ok := old.Operations().(*Node); ok && child.snapshot().ID == e.ID {
			return old
		}
	}
	return n.NewInode(ctx, n.f.makeNode(e), stable(e))
}

func (n *Node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	entries, err := n.f.list(ctx, n, false)
	if err != nil {
		return nil, n.failure("lookup", err)
	}
	for _, e := range entries {
		if e.Name == name {
			child := n.child(ctx, e)
			node := child.Operations().(*Node)
			node.mu.Lock()
			node.fillLocked(&out.Attr)
			node.mu.Unlock()
			return child, 0
		}
	}
	return nil, syscall.ENOENT
}

func (n *Node) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	entries, err := n.f.list(ctx, n, false)
	if err != nil {
		return nil, n.failure("readdir", err)
	}
	stream := make([]fuse.DirEntry, 0, len(entries))
	for _, e := range entries {
		attr := stable(e)
		stream = append(stream, fuse.DirEntry{Name: e.Name, Mode: attr.Mode, Ino: attr.Ino})
	}
	return fs.NewListDirStream(stream), 0
}

func (n *Node) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	writable := flags&syscall.O_ACCMODE != syscall.O_RDONLY
	if writable && n.f.opts.ReadOnly {
		return nil, 0, syscall.EROFS
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.entry.Dir {
		return nil, 0, syscall.EISDIR
	}
	if n.refs == 0 && !n.dirty && !n.unlinked {
		entry, err := n.f.backend.Stat(ctx, n.entry)
		if err != nil {
			return nil, 0, n.failure("open", err)
		}
		n.entry = entry
		n.statAt = time.Now()
	}
	if writable && !n.unlinked {
		ok, err := n.f.backend.Can(ctx, n.entry.ID, "modify")
		if err != nil {
			return nil, 0, n.failure("check modify permission", err)
		}
		if !ok {
			return nil, 0, syscall.EACCES
		}
	}
	if writable {
		if err := n.ensureCacheLocked(ctx, flags&syscall.O_TRUNC != 0); err != nil {
			return nil, 0, n.failure("prepare write cache", err)
		}
		if flags&syscall.O_TRUNC != 0 {
			if err := n.resizeLocked(0); err != nil {
				return nil, 0, n.failure("truncate", err)
			}
		}
	}
	n.refs++
	return &Handle{node: n, writable: writable, append: flags&syscall.O_APPEND != 0}, 0, 0
}
