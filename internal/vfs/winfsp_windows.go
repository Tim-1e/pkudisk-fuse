package vfs

import (
	"bytes"
	"context"
	"errors"
	"hash/fnv"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/thezzisu/pkudisk-fuse/internal/anyshare"
	"github.com/thezzisu/pkudisk-fuse/internal/localfile"
	"github.com/winfsp/cgofuse/fuse"
	"golang.org/x/sys/windows"
)

// inodeBase is supplied by go-fuse on Unix. WinFsp addresses the same cached
// Node state through paths and file handles rather than kernel inode objects.
type inodeBase struct{}

type winHandle struct {
	node     *Node
	writable bool
	append   bool
	entries  []anyshare.Entry
}

type WinFsp struct {
	fuse.FileSystemBase
	core    *FileSystem
	ctx     context.Context
	mu      sync.Mutex
	paths   map[string]*Node
	handles map[uint64]*winHandle
	next    uint64
	Ready   chan struct{}
}

func NewWinFsp(ctx context.Context, core *FileSystem) *WinFsp {
	return &WinFsp{core: core, ctx: ctx, paths: map[string]*Node{"/": core.root}, handles: map[uint64]*winHandle{}, Ready: make(chan struct{})}
}

// WinFspMountOptions binds all exposed POSIX owners to the mounting Windows
// user's SID. WinFsp documents -1 as the mounting user's UID/GID.
func WinFspMountOptions() []string { return []string{"-o", "uid=-1,gid=-1"} }

func (w *WinFsp) Init() { close(w.Ready) }

func (w *WinFsp) operation() (context.Context, context.CancelFunc) {
	return context.WithTimeout(w.ctx, w.core.opts.IOTimeout)
}

func winError(err error) int {
	if err == nil {
		return 0
	}
	switch {
	case errors.Is(err, anyshare.ErrNotFound), errors.Is(err, os.ErrNotExist):
		return -fuse.ENOENT
	case errors.Is(err, anyshare.ErrExists), errors.Is(err, os.ErrExist):
		return -fuse.EEXIST
	case errors.Is(err, anyshare.ErrPermission), errors.Is(err, anyshare.ErrPending), errors.Is(err, os.ErrPermission):
		return -fuse.EACCES
	case errors.Is(err, anyshare.ErrConflict):
		return -fuse.EBUSY
	case errors.Is(err, anyshare.ErrQuota), errors.Is(err, syscall.ENOSPC), errors.Is(err, windows.ERROR_DISK_FULL):
		return -fuse.ENOSPC
	case errors.Is(err, anyshare.ErrSizeLimit), errors.Is(err, syscall.EFBIG):
		return -fuse.EFBIG
	case errors.Is(err, context.Canceled):
		return -fuse.EINTR
	case errors.Is(err, context.DeadlineExceeded):
		return -fuse.ETIMEDOUT
	default:
		return -fuse.EIO
	}
}

func (w *WinFsp) failure(operation string, err error) int {
	if err != nil {
		w.core.opts.Logger.Printf("%s: %v", operation, err)
	}
	return winError(err)
}

func winPath(value string) (string, bool) {
	if !strings.HasPrefix(value, "/") || strings.ContainsRune(value, '\x00') {
		return "", false
	}
	for _, part := range strings.Split(strings.Trim(value, "/"), "/") {
		if part == "" {
			continue
		}
		if !localfile.ValidName(part) {
			return "", false
		}
	}
	return path.Clean(value), true
}

func (w *WinFsp) remember(value string, entry anyshare.Entry) *Node {
	w.mu.Lock()
	node := w.paths[value]
	w.mu.Unlock()
	if node != nil {
		node.mu.Lock()
		if !node.unlinked && node.entry.ID == entry.ID {
			if !node.dirty && node.refs == 0 {
				node.entry = entry
			}
			node.mu.Unlock()
			return node
		}
		node.mu.Unlock()
	}
	fresh := w.core.makeNode(entry)
	w.mu.Lock()
	if w.paths[value] != node {
		w.mu.Unlock()
		return w.remember(value, entry)
	}
	w.paths[value] = fresh
	w.mu.Unlock()
	return fresh
}

func (w *WinFsp) resolve(ctx context.Context, value string) (*Node, error) {
	clean, ok := winPath(value)
	if !ok {
		return nil, anyshare.ErrNotFound
	}
	if clean == "/" {
		return w.core.root, nil
	}
	current, prefix := w.core.root, ""
	for _, name := range strings.Split(strings.TrimPrefix(clean, "/"), "/") {
		if !current.snapshot().Dir {
			return nil, anyshare.ErrNotFound
		}
		entries, err := w.core.list(ctx, current, false)
		if err != nil {
			return nil, err
		}
		found := false
		for _, entry := range entries {
			if entry.Name == name {
				prefix += "/" + name
				current = w.remember(prefix, entry)
				found = true
				break
			}
		}
		if !found {
			return nil, anyshare.ErrNotFound
		}
	}
	return current, nil
}

func (w *WinFsp) handle(fh uint64) *winHandle {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.handles[fh]
}

func (w *WinFsp) addHandle(handle *winHandle) uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.next++
	w.handles[w.next] = handle
	return w.next
}

func (w *WinFsp) node(ctx context.Context, value string, fh uint64) (*Node, error) {
	if fh != ^uint64(0) {
		if handle := w.handle(fh); handle != nil {
			return handle.node, nil
		}
	}
	return w.resolve(ctx, value)
}

func winStat(entry anyshare.Entry, mode uint32) fuse.Stat_t {
	hash := fnv.New64a()
	hash.Write([]byte(entry.ID))
	size := max(entry.Size, 0)
	mtime := entry.Modified
	if mtime.IsZero() {
		mtime = time.Unix(0, 0)
	}
	created := entry.Created
	if created.IsZero() {
		created = mtime
	}
	typ, links := uint32(fuse.S_IFREG), uint32(1)
	if entry.Dir {
		typ, links = fuse.S_IFDIR, 2
	}
	return fuse.Stat_t{Ino: max(hash.Sum64(), 2), Mode: typ | mode, Nlink: links, Size: size,
		Blksize: 4096, Blocks: (size / 512) + int64(boolInt(size%512 != 0)),
		Atim: fuse.NewTimespec(mtime), Mtim: fuse.NewTimespec(mtime), Ctim: fuse.NewTimespec(mtime), Birthtim: fuse.NewTimespec(created)}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (w *WinFsp) Getattr(value string, stat *fuse.Stat_t, fh uint64) int {
	ctx, cancel := w.operation()
	defer cancel()
	node, err := w.node(ctx, value, fh)
	if err != nil {
		return winError(err)
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	if !node.synthetic && !node.dirty && !node.unlinked && node.refs == 0 && time.Since(node.statAt) >= w.core.opts.TTL {
		entry, err := w.core.backend.Stat(ctx, node.entry)
		if err != nil {
			return winError(err)
		}
		node.entry, node.statAt = entry, time.Now()
	}
	mode := node.mode
	if w.core.opts.ReadOnly || node.synthetic {
		mode &= 0555
	}
	*stat = winStat(node.entry, mode)
	return 0
}

func (w *WinFsp) Open(value string, flags int) (int, uint64) {
	ctx, cancel := w.operation()
	defer cancel()
	node, err := w.resolve(ctx, value)
	if err != nil {
		return winError(err), ^uint64(0)
	}
	writable := flags&fuse.O_ACCMODE != fuse.O_RDONLY
	if writable && w.core.opts.ReadOnly {
		return -fuse.EROFS, ^uint64(0)
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	if node.entry.Dir {
		return -fuse.EISDIR, ^uint64(0)
	}
	if node.refs == 0 && !node.dirty {
		entry, err := w.core.backend.Stat(ctx, node.entry)
		if err != nil {
			return winError(err), ^uint64(0)
		}
		node.entry, node.statAt = entry, time.Now()
	}
	if writable {
		allowed, err := w.core.backend.Can(ctx, node.entry.ID, "modify")
		if err != nil || !allowed {
			if err == nil {
				err = anyshare.ErrPermission
			}
			return winError(err), ^uint64(0)
		}
		if err := node.ensureCacheLocked(ctx, flags&fuse.O_TRUNC != 0); err != nil {
			return winError(err), ^uint64(0)
		}
		if flags&fuse.O_TRUNC != 0 {
			if err := node.resizeLocked(0); err != nil {
				return winError(err), ^uint64(0)
			}
		}
	}
	node.refs++
	return 0, w.addHandle(&winHandle{node: node, writable: writable, append: flags&fuse.O_APPEND != 0})
}

func (w *WinFsp) Read(_ string, data []byte, off int64, fh uint64) int {
	handle := w.handle(fh)
	if handle == nil {
		return -fuse.EBADF
	}
	if off < 0 {
		return -fuse.EINVAL
	}
	node := handle.node
	node.mu.Lock()
	defer node.mu.Unlock()
	if off >= node.entry.Size {
		return 0
	}
	data = data[:min(int64(len(data)), node.entry.Size-off)]
	var n int
	var err error
	if node.cache != nil {
		n, err = node.cache.ReadAt(data, off)
	} else {
		ctx, cancel := w.operation()
		defer cancel()
		n, err = w.core.backend.ReadAt(ctx, node.entry, data, off)
	}
	if err != nil && err != io.EOF {
		return w.failure("read", err)
	}
	if n != len(data) {
		return -fuse.EIO
	}
	return n
}

func (w *WinFsp) Write(_ string, data []byte, off int64, fh uint64) int {
	handle := w.handle(fh)
	if handle == nil || !handle.writable {
		return -fuse.EBADF
	}
	node := handle.node
	node.mu.Lock()
	defer node.mu.Unlock()
	if handle.append {
		off = node.entry.Size
	}
	if off < 0 || off > int64(^uint64(0)>>1)-int64(len(data)) {
		return -fuse.EFBIG
	}
	if err := node.reserveLocked(max(node.entry.Size, off+int64(len(data)))); err != nil {
		return winError(err)
	}
	n, err := node.cache.WriteAt(data, off)
	if n > 0 {
		node.entry.Size = max(node.entry.Size, off+int64(n))
		node.entry.Modified, node.dirty = time.Now(), true
		if journalErr := node.journalLocked(); err == nil {
			err = journalErr
		}
	}
	if err != nil {
		return w.failure("write", err)
	}
	return n
}

func (w *WinFsp) Flush(_ string, fh uint64) int {
	handle := w.handle(fh)
	if handle == nil {
		return -fuse.EBADF
	}
	if !handle.writable {
		return 0
	}
	ctx, cancel := w.operation()
	defer cancel()
	handle.node.mu.Lock()
	defer handle.node.mu.Unlock()
	return w.failure("upload on close", handle.node.flushLocked(ctx))
}

func (w *WinFsp) Fsync(value string, _ bool, fh uint64) int { return w.Flush(value, fh) }

func (w *WinFsp) Release(_ string, fh uint64) int {
	w.mu.Lock()
	handle := w.handles[fh]
	delete(w.handles, fh)
	w.mu.Unlock()
	if handle == nil {
		return -fuse.EBADF
	}
	handle.node.mu.Lock()
	defer handle.node.mu.Unlock()
	handle.node.refs--
	if handle.node.refs == 0 && !handle.node.dirty {
		handle.node.disposeLocked()
	}
	return 0
}

func (w *WinFsp) parent(ctx context.Context, value string) (*Node, string, int) {
	clean, ok := winPath(value)
	if !ok || clean == "/" {
		return nil, "", -fuse.EINVAL
	}
	node, err := w.resolve(ctx, path.Dir(clean))
	if err != nil {
		return nil, "", winError(err)
	}
	if !node.snapshot().Dir {
		return nil, "", -fuse.ENOTDIR
	}
	if w.core.opts.ReadOnly {
		return nil, "", -fuse.EROFS
	}
	if node.synthetic {
		return nil, "", -fuse.EPERM
	}
	return node, path.Base(clean), 0
}

func (w *WinFsp) Create(value string, flags int, mode uint32) (int, uint64) {
	ctx, cancel := w.operation()
	defer cancel()
	w.core.opMu.Lock()
	defer w.core.opMu.Unlock()
	parent, name, code := w.parent(ctx, value)
	if code != 0 {
		return code, ^uint64(0)
	}
	allowed, err := w.core.backend.Can(ctx, parent.snapshot().ID, "create")
	if err != nil || !allowed {
		if err == nil {
			err = anyshare.ErrPermission
		}
		return winError(err), ^uint64(0)
	}
	entry, err := w.core.backend.Upload(ctx, parent.snapshot().ID, name, nil, bytes.NewReader(nil), 0, time.Now())
	if err != nil {
		return w.failure("create file", err), ^uint64(0)
	}
	entry.Modified, entry.Created = time.Now(), time.Now()
	node := w.remember(path.Clean(value), entry)
	node.mu.Lock()
	defer node.mu.Unlock()
	node.mode = mode & 0777
	if err := node.ensureCacheLocked(ctx, true); err != nil {
		return winError(err), ^uint64(0)
	}
	node.refs++
	w.core.invalidate()
	return 0, w.addHandle(&winHandle{node: node, writable: flags&fuse.O_ACCMODE != fuse.O_RDONLY, append: flags&fuse.O_APPEND != 0})
}

func (w *WinFsp) Mkdir(value string, mode uint32) int {
	ctx, cancel := w.operation()
	defer cancel()
	w.core.opMu.Lock()
	defer w.core.opMu.Unlock()
	parent, name, code := w.parent(ctx, value)
	if code != 0 {
		return code
	}
	entry, err := w.core.backend.Mkdir(ctx, parent.snapshot().ID, name)
	if err != nil {
		return winError(err)
	}
	node := w.remember(path.Clean(value), entry)
	node.mu.Lock()
	node.mode = mode & 0777
	node.mu.Unlock()
	w.core.invalidate()
	return 0
}

func (w *WinFsp) Truncate(value string, size int64, fh uint64) int {
	if w.core.opts.ReadOnly {
		return -fuse.EROFS
	}
	if size < 0 {
		return -fuse.EINVAL
	}
	if handle := w.handle(fh); handle != nil && !handle.writable {
		return -fuse.EBADF
	}
	ctx, cancel := w.operation()
	defer cancel()
	node, err := w.node(ctx, value, fh)
	if err != nil {
		return winError(err)
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	if node.entry.Dir {
		return -fuse.EISDIR
	}
	if !node.unlinked {
		allowed, err := w.core.backend.Can(ctx, node.entry.ID, "modify")
		if err != nil || !allowed {
			if err == nil {
				err = anyshare.ErrPermission
			}
			return winError(err)
		}
	}
	if err := node.ensureCacheLocked(ctx, size == 0); err != nil {
		return winError(err)
	}
	if err := node.resizeLocked(size); err != nil {
		return winError(err)
	}
	if node.refs == 0 {
		if err := node.flushLocked(ctx); err != nil {
			return w.failure("commit truncate", err)
		}
		node.disposeLocked()
	}
	return 0
}

func (w *WinFsp) remove(value string, directory bool) int {
	ctx, cancel := w.operation()
	defer cancel()
	w.core.opMu.Lock()
	defer w.core.opMu.Unlock()
	if _, _, code := w.parent(ctx, value); code != 0 {
		return code
	}
	node, err := w.resolve(ctx, value)
	if err != nil {
		return winError(err)
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	if node.entry.Dir != directory {
		if directory {
			return -fuse.ENOTDIR
		}
		return -fuse.EISDIR
	}
	if directory {
		entries, err := w.core.backend.List(ctx, node.entry.ID)
		if err != nil {
			return winError(err)
		}
		if len(entries) > 0 {
			return -fuse.ENOTEMPTY
		}
	} else if node.refs > 0 {
		if err := node.ensureCacheLocked(ctx, false); err != nil {
			return winError(err)
		}
	}
	if err := w.core.backend.Delete(ctx, node.entry); err != nil {
		return winError(err)
	}
	node.unlinked = true
	w.core.invalidate()
	return 0
}

func (w *WinFsp) Unlink(value string) int { return w.remove(value, false) }
func (w *WinFsp) Rmdir(value string) int  { return w.remove(value, true) }

func (w *WinFsp) Rename(oldPath, newPath string) int {
	ctx, cancel := w.operation()
	defer cancel()
	w.core.opMu.Lock()
	defer w.core.opMu.Unlock()
	oldParent, _, code := w.parent(ctx, oldPath)
	if code != 0 {
		return code
	}
	newParent, newName, code := w.parent(ctx, newPath)
	if code != 0 {
		return code
	}
	if path.Clean(oldPath) == path.Clean(newPath) {
		return 0
	}
	oldParentEntry, newParentEntry := oldParent.snapshot(), newParent.snapshot()
	if anyshare.LibraryID(oldParentEntry.ID) != anyshare.LibraryID(newParentEntry.ID) {
		return -fuse.EXDEV
	}
	source, err := w.resolve(ctx, oldPath)
	if err != nil {
		return winError(err)
	}
	target, err := w.resolve(ctx, newPath)
	if err != nil && !errors.Is(err, anyshare.ErrNotFound) {
		return winError(err)
	}
	if target == source {
		return 0
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	old := source.entry
	if old.Dir && (newParentEntry.ID == old.ID || strings.HasPrefix(newParentEntry.ID, old.ID+"/")) {
		return -fuse.EINVAL
	}
	if target != nil && target != source {
		target.mu.Lock()
		defer target.mu.Unlock()
		if target.entry.Dir != old.Dir {
			if target.entry.Dir {
				return -fuse.EISDIR
			}
			return -fuse.ENOTDIR
		}
		if target.entry.Dir {
			entries, err := w.core.backend.List(ctx, target.entry.ID)
			if err != nil {
				return winError(err)
			}
			if len(entries) > 0 {
				return -fuse.ENOTEMPTY
			}
		} else if target.refs > 0 {
			if err := target.ensureCacheLocked(ctx, false); err != nil {
				return winError(err)
			}
		}
	}
	if err := source.flushLocked(ctx); err != nil {
		return winError(err)
	}
	old = source.entry
	var updated anyshare.Entry
	if oldParentEntry.ID == newParentEntry.ID {
		updated, err = w.core.backend.Rename(ctx, old, newName, target != nil)
	} else {
		updated, err = w.core.moveAndRename(ctx, old, oldParentEntry.ID, newParentEntry.ID, newName, target != nil)
	}
	if err != nil {
		w.core.invalidate()
		return w.failure("rename", err)
	}
	updated.Created, updated.Modified = old.Created, old.Modified
	source.entry, source.statAt = updated, time.Now()
	if target != nil && target != source {
		target.unlinked = true
	}
	w.mu.Lock()
	oldPath, newPath = path.Clean(oldPath), path.Clean(newPath)
	children := map[string]*Node{}
	for value, node := range w.paths {
		if strings.HasPrefix(value, oldPath+"/") {
			children[value] = node
		}
	}
	for value, node := range children {
		delete(w.paths, value)
		w.paths[newPath+strings.TrimPrefix(value, oldPath)] = node
	}
	delete(w.paths, oldPath)
	w.paths[newPath] = source
	w.mu.Unlock()
	for _, node := range children {
		if old.ID != updated.ID {
			node.mu.Lock()
			node.entry.ID = updated.ID + strings.TrimPrefix(node.entry.ID, old.ID)
			if node.dirty {
				if err := node.journalLocked(); err != nil {
					w.failure("rebase write journal", err)
				}
			}
			node.mu.Unlock()
		}
	}
	w.core.invalidate()
	return 0
}

func (w *WinFsp) Opendir(value string) (int, uint64) {
	ctx, cancel := w.operation()
	defer cancel()
	node, err := w.resolve(ctx, value)
	if err != nil {
		return winError(err), ^uint64(0)
	}
	if !node.snapshot().Dir {
		return -fuse.ENOTDIR, ^uint64(0)
	}
	entries, err := w.core.list(ctx, node, false)
	if err != nil {
		return winError(err), ^uint64(0)
	}
	node.mu.Lock()
	node.refs++
	node.mu.Unlock()
	return 0, w.addHandle(&winHandle{node: node, entries: entries})
}

func (w *WinFsp) Readdir(value string, fill func(string, *fuse.Stat_t, int64) bool, off int64, fh uint64) int {
	handle := w.handle(fh)
	if handle == nil || off < 0 {
		return -fuse.EBADF
	}
	if !fill(".", nil, 0) || !fill("..", nil, 0) {
		return 0
	}
	for i, entry := range handle.entries {
		if int64(i) < off || !localfile.ValidName(entry.Name) {
			continue
		}
		mode := uint32(0600)
		if entry.Dir {
			mode = 0700
		}
		if w.core.opts.ReadOnly {
			mode &= 0555
		}
		stat := winStat(entry, mode)
		w.remember(path.Join(value, entry.Name), entry)
		if !fill(entry.Name, &stat, int64(i+1)) {
			break
		}
	}
	return 0
}

func (w *WinFsp) Releasedir(value string, fh uint64) int { return w.Release(value, fh) }
func (w *WinFsp) Fsyncdir(string, bool, uint64) int      { return 0 }

func (w *WinFsp) Statfs(_ string, stat *fuse.Statfs_t) int {
	// Use the local cache volume's actual free space rather than inventing a
	// remote quota. The server remains authoritative when publishing writes.
	p, err := windows.UTF16PtrFromString(w.core.cacheDir)
	if err != nil {
		return winError(err)
	}
	var available, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &available, &total, &free); err != nil {
		return winError(err)
	}
	*stat = fuse.Statfs_t{Bsize: 4096, Frsize: 4096, Blocks: total / 4096, Bfree: free / 4096, Bavail: available / 4096, Namemax: 255}
	return 0
}

func (w *WinFsp) Access(value string, mask uint32) int {
	if mask&2 != 0 && w.core.opts.ReadOnly {
		return -fuse.EROFS
	}
	ctx, cancel := w.operation()
	defer cancel()
	node, err := w.resolve(ctx, value)
	if err != nil {
		return winError(err)
	}
	if mask&2 != 0 && node.synthetic {
		return -fuse.EACCES
	}
	return 0
}

func (w *WinFsp) Chmod(value string, mode uint32) int {
	if w.core.opts.ReadOnly {
		return -fuse.EROFS
	}
	ctx, cancel := w.operation()
	defer cancel()
	node, err := w.resolve(ctx, value)
	if err != nil {
		return winError(err)
	}
	node.mu.Lock()
	node.mode = mode & 0777
	node.mu.Unlock()
	return 0
}

func (w *WinFsp) Chflags(value string, _ uint32) int {
	// DOS archive/hidden flags have no matching AnyShare metadata.
	return w.Access(value, 2)
}

func (w *WinFsp) Utimens(value string, times []fuse.Timespec) int {
	if w.core.opts.ReadOnly {
		return -fuse.EROFS
	}
	ctx, cancel := w.operation()
	defer cancel()
	node, err := w.resolve(ctx, value)
	if err != nil {
		return winError(err)
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	mtime := time.Now()
	if len(times) > 1 {
		mtime = times[1].Time()
	}
	if node.entry.Dir {
		node.entry.Modified = mtime
		return 0
	}
	if err := node.ensureCacheLocked(ctx, false); err != nil {
		return winError(err)
	}
	node.entry.Modified, node.dirty = mtime, true
	if err := node.journalLocked(); err != nil {
		return winError(err)
	}
	if node.refs == 0 {
		if err := node.flushLocked(ctx); err != nil {
			return w.failure("commit timestamp", err)
		}
		node.disposeLocked()
	}
	return 0
}

var _ fuse.FileSystemInterface = (*WinFsp)(nil)
