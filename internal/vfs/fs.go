// Package vfs maps AnyShare libraries to Linux FUSE inodes. Writes use a shared
// per-inode disk cache and are published synchronously on flush/fsync.
package vfs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"pkudisk-fuse/internal/anyshare"
)

type Backend interface {
	Roots(context.Context) ([]anyshare.Entry, error)
	List(context.Context, string) ([]anyshare.Entry, error)
	Stat(context.Context, anyshare.Entry) (anyshare.Entry, error)
	Can(context.Context, string, string) (bool, error)
	ReadAt(context.Context, anyshare.Entry, []byte, int64) (int, error)
	Download(context.Context, anyshare.Entry, io.Writer) error
	Upload(context.Context, string, string, *anyshare.Entry, io.ReaderAt, int64, time.Time) (anyshare.Entry, error)
	Mkdir(context.Context, string, string) (anyshare.Entry, error)
	Delete(context.Context, anyshare.Entry) error
	Rename(context.Context, anyshare.Entry, string, bool) (anyshare.Entry, error)
	Move(context.Context, anyshare.Entry, string) (anyshare.Entry, error)
}

type Options struct {
	Root       *anyshare.Entry
	ReadOnly   bool
	CacheDir   string
	CacheLimit int64
	TTL        time.Duration
	IOTimeout  time.Duration
	Logger     *log.Logger
}
type cachedList struct {
	entries []anyshare.Entry
	expires time.Time
}

type FileSystem struct {
	backend    Backend
	opts       Options
	cacheDir   string
	opMu       sync.Mutex
	listMu     sync.Mutex
	listCache  map[string]cachedList
	budgetMu   sync.Mutex
	cacheBytes int64
	nodes      sync.Map
	root       *Node
}

type Node struct {
	fs.Inode
	f         *FileSystem
	mu        sync.Mutex
	entry     anyshare.Entry
	synthetic bool
	cache     *os.File
	cachePath string
	charged   int64
	dirty     bool
	unlinked  bool
	refs      int
	statAt    time.Time
	mode      uint32
}

func New(backend Backend, opts Options) (*FileSystem, error) {
	if backend == nil {
		return nil, errors.New("missing backend")
	}
	if opts.TTL < 0 || opts.CacheLimit < 0 {
		return nil, errors.New("cache limits must be nonnegative")
	}
	if opts.IOTimeout <= 0 {
		opts.IOTimeout = 10 * time.Minute
	}
	if opts.Logger == nil {
		opts.Logger = log.New(os.Stderr, "pkudisk-fuse: ", log.LstdFlags)
	}
	if opts.CacheDir == "" {
		return nil, errors.New("a private cache directory is required")
	}
	if err := os.MkdirAll(opts.CacheDir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Stat(opts.CacheDir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("cache directory must have mode 0700")
	}
	cache, err := os.MkdirTemp(opts.CacheDir, "mount-")
	if err != nil {
		return nil, err
	}
	f := &FileSystem{backend: backend, opts: opts, cacheDir: cache, listCache: map[string]cachedList{}}
	if opts.Root != nil {
		if !opts.Root.Dir {
			os.Remove(cache)
			return nil, syscall.ENOTDIR
		}
		f.root = f.makeNode(*opts.Root)
	} else {
		f.root = f.makeNode(anyshare.Entry{Name: "PKU NetDisk", Dir: true, Size: -1})
		f.root.synthetic = true
	}
	return f, nil
}
func (f *FileSystem) Root() *Node       { return f.root }
func (f *FileSystem) CachePath() string { return f.cacheDir }
func (f *FileSystem) makeNode(e anyshare.Entry) *Node {
	n := &Node{f: f, entry: e, statAt: time.Now()}
	if e.Dir {
		n.mode = 0700
	} else {
		n.mode = 0600
	}
	f.nodes.Store(n, true)
	return n
}
func (n *Node) snapshot() anyshare.Entry { n.mu.Lock(); defer n.mu.Unlock(); return n.entry }

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

func (f *FileSystem) list(ctx context.Context, n *Node, fresh bool) ([]anyshare.Entry, error) {
	e := n.snapshot()
	f.listMu.Lock()
	hit, ok := f.listCache[e.ID]
	f.listMu.Unlock()
	if !fresh && ok && time.Now().Before(hit.expires) {
		return append([]anyshare.Entry(nil), hit.entries...), nil
	}
	var entries []anyshare.Entry
	var err error
	if n.synthetic {
		entries, err = f.backend.Roots(ctx)
	} else {
		entries, err = f.backend.List(ctx, e.ID)
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !anyshare.ValidName(e.Name) || e.ID == "" {
			return nil, errors.New("server returned an unsupported directory entry")
		}
	}
	f.listMu.Lock()
	f.listCache[e.ID] = cachedList{append([]anyshare.Entry(nil), entries...), time.Now().Add(f.opts.TTL)}
	f.listMu.Unlock()
	return entries, nil
}
func (f *FileSystem) invalidate() {
	f.listMu.Lock()
	f.listCache = map[string]cachedList{}
	f.listMu.Unlock()
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
	return &Handle{node: n, writable: writable, append: flags&syscall.O_APPEND != 0}, fuse.FOPEN_DIRECT_IO, 0
}

func (n *Node) reserveLocked(size int64) error {
	if size < 0 {
		return syscall.EFBIG
	}
	f := n.f
	f.budgetMu.Lock()
	defer f.budgetMu.Unlock()
	delta := size - n.charged
	if delta > 0 && f.opts.CacheLimit > 0 && delta > f.opts.CacheLimit-f.cacheBytes {
		return syscall.ENOSPC
	}
	f.cacheBytes += delta
	n.charged = size
	return nil
}

func (n *Node) ensureCacheLocked(ctx context.Context, truncate bool) error {
	if n.cache != nil {
		return nil
	}
	size := n.entry.Size
	if truncate {
		size = 0
	}
	if err := n.reserveLocked(size); err != nil {
		return err
	}
	file, err := os.CreateTemp(n.f.cacheDir, "file-*.data")
	if err != nil {
		n.reserveLocked(0)
		return err
	}
	if !truncate && size > 0 {
		err = n.f.backend.Download(ctx, n.entry, file)
	}
	if err != nil {
		file.Close()
		os.Remove(file.Name())
		n.reserveLocked(0)
		return err
	}
	n.cache = file
	n.cachePath = file.Name()
	return nil
}

type RecoveryRecord struct {
	Entry   anyshare.Entry `json:"entry"`
	Parent  string         `json:"parent"`
	Data    string         `json:"data"`
	Updated time.Time      `json:"updated"`
}

func (n *Node) journalLocked() error {
	data, err := json.MarshalIndent(RecoveryRecord{Entry: n.entry, Parent: anyshare.ParentID(n.entry.ID), Data: filepath.Base(n.cachePath), Updated: time.Now()}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(n.f.cacheDir, "journal-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), n.cachePath+".json")
	}
	return err
}

func (n *Node) resizeLocked(size int64) error {
	if err := n.reserveLocked(size); err != nil {
		return err
	}
	if err := n.cache.Truncate(size); err != nil {
		return err
	}
	n.entry.Size = size
	n.entry.Modified = time.Now()
	n.dirty = true
	return n.journalLocked()
}

func (n *Node) flushLocked(ctx context.Context) error {
	if !n.dirty {
		return nil
	}
	if n.unlinked {
		n.dirty = false
		os.Remove(n.cachePath + ".json")
		return nil
	}
	if err := n.cache.Sync(); err != nil {
		return err
	}
	if err := n.journalLocked(); err != nil {
		return err
	}
	old := n.entry
	entry, err := n.f.backend.Upload(ctx, anyshare.ParentID(old.ID), old.Name, &old, n.cache, old.Size, old.Modified)
	if err != nil {
		n.f.opts.Logger.Printf("uncommitted write retained at %s", n.cachePath)
		return err
	}
	entry.Created, entry.Modified = old.Created, old.Modified
	n.entry = entry
	n.dirty = false
	n.statAt = time.Now()
	os.Remove(n.cachePath + ".json")
	n.f.invalidate()
	return nil
}

func (n *Node) disposeLocked() {
	if n.cache == nil {
		return
	}
	n.cache.Close()
	n.cache = nil
	if !n.dirty {
		os.Remove(n.cachePath)
		os.Remove(n.cachePath + ".json")
	}
	n.reserveLocked(0)
}

func (f *FileSystem) Close() error {
	dirty := 0
	f.nodes.Range(func(key, value any) bool {
		n := key.(*Node)
		n.mu.Lock()
		if n.dirty {
			dirty++
		}
		n.disposeLocked()
		n.mu.Unlock()
		return true
	})
	os.Remove(f.cacheDir)
	if dirty > 0 {
		return fmtErrorRecovery(dirty, f.cacheDir)
	}
	return nil
}
func fmtErrorRecovery(count int, path string) error {
	return errors.New("uncommitted cached writes remain in " + path + "; use the recovery command before removing the cache")
}

func RandomName() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return ".pkudisk-fuse-move-" + hex.EncodeToString(b[:])
}

func RecoveryFiles(dir string) ([]RecoveryRecord, error) {
	var result []RecoveryRecord
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var r RecoveryRecord
		if err = json.Unmarshal(b, &r); err != nil {
			return err
		}
		if filepath.Base(r.Data) != r.Data || r.Data == "" {
			return errors.New("invalid recovery data path")
		}
		r.Data = filepath.Join(filepath.Dir(path), r.Data)
		result = append(result, r)
		return nil
	})
	sort.Slice(result, func(i, j int) bool { return result[i].Data < result[j].Data })
	return result, err
}
