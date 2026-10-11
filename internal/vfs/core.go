// Package vfs shares remote namespace, cached reads/writes and recovery between
// go-fuse on Unix and WinFsp on Windows.
package vfs

import (
	"container/list"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/thezzisu/pkudisk-fuse/internal/anyshare"
	"github.com/thezzisu/pkudisk-fuse/internal/localfile"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
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
	backend     Backend
	opts        Options
	cacheDir    string
	opMu        sync.Mutex
	listMu      sync.Mutex
	listCache   map[string]cachedList
	budgetMu    sync.Mutex
	cacheBytes  int64
	readFiles   map[cacheKey]*list.Element
	readLRU     list.List
	readFlights map[cacheKey]*readFlight
	nodes       sync.Map
	root        *Node
}

type Node struct {
	inodeBase
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
	if err := localfile.EnsurePrivateDir(opts.CacheDir); err != nil {
		return nil, err
	}
	cache, err := os.MkdirTemp(opts.CacheDir, "mount-")
	if err != nil {
		return nil, err
	}
	f := &FileSystem{backend: backend, opts: opts, cacheDir: cache, listCache: map[string]cachedList{}, readFiles: make(map[cacheKey]*list.Element), readFlights: make(map[cacheKey]*readFlight)}
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

func (n *Node) reserveLocked(size int64) error {
	if size < 0 {
		return syscall.EFBIG
	}
	f := n.f
	f.budgetMu.Lock()
	defer f.budgetMu.Unlock()
	delta := size - n.charged
	if delta > 0 && !f.cacheRoomLocked(delta) {
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
	if !truncate {
		if file, size := n.f.takeCachedFile(n.entry); file != nil {
			n.cache, n.cachePath, n.charged = file, file.Name(), size
			return nil
		}
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
		err = localfile.Install(tmp.Name(), n.cachePath+".json", true)
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
		os.Remove(n.cachePath + ".json")
		if !n.unlinked && n.entry.Rev != "" {
			n.f.retainCachedFile(n.entry, n.cachePath, n.charged)
			n.charged = 0 // The clean-cache LRU now owns this allocation.
			return
		}
		os.Remove(n.cachePath)
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
	f.clearReadCache()
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
