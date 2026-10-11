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
	"github.com/thezzisu/pkudisk-fuse/internal/diagnostic"
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
	Read       *ReadOptions
	TTL        time.Duration
	IOTimeout  time.Duration
	Logger     *log.Logger
	Trace      *diagnostic.Logger
}
type cachedList struct {
	entries []anyshare.Entry
	expires time.Time
}

type listFlight struct {
	done    chan struct{}
	entries []anyshare.Entry
	err     error
}

type statFlight struct {
	done chan struct{}
	err  error
}

type FileSystem struct {
	backend     Backend
	opts        Options
	cacheDir    string
	opMu        sync.Mutex
	listMu      sync.Mutex
	listCache   map[string]cachedList
	listFlights map[string]*listFlight
	budgetMu    sync.Mutex
	cacheBytes  int64
	readBytes   int64
	readSlots   chan struct{}
	readFiles   map[cacheKey]*list.Element
	readLRU     list.List
	readFlights map[cacheKey]*readFlight
	nodes       sync.Map
	root        *Node
}

type Node struct {
	inodeBase
	traceName  string // Initial name, readable before taking the node lock.
	f          *FileSystem
	mu         sync.Mutex
	entry      anyshare.Entry
	synthetic  bool
	cache      *os.File
	cachePath  string
	charged    int64
	dirty      bool
	unlinked   bool
	refs       int
	statAt     time.Time
	statFlight *statFlight
	mode       uint32
	readEnd    int64
	readSize   int64
	readRev    string
}

func New(backend Backend, opts Options) (*FileSystem, error) {
	if backend == nil {
		return nil, errors.New("missing backend")
	}
	if opts.TTL < 0 || opts.CacheLimit < 0 {
		return nil, errors.New("cache limits must be nonnegative")
	}
	read := DefaultReadOptions()
	if opts.Read != nil {
		read = *opts.Read
	}
	if err := read.Validate(); err != nil {
		return nil, err
	}
	opts.Read = &read
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
	f := &FileSystem{backend: backend, opts: opts, cacheDir: cache, listCache: map[string]cachedList{}, listFlights: make(map[string]*listFlight), readFiles: make(map[cacheKey]*list.Element), readFlights: make(map[cacheKey]*readFlight), readSlots: make(chan struct{}, read.Concurrency)}
	opts.Trace.Printf(context.Background(), "mount policy: whole-file=%s block=%s read-ahead=%s concurrency=%d disk-budget=%s metadata-TTL=%s I/O-timeout=%s read-only=%t", diagnostic.Size(read.WholeFile), diagnostic.Size(read.BlockSize), diagnostic.Size(read.ReadAhead), read.Concurrency, diagnostic.Size(opts.CacheLimit), opts.TTL, opts.IOTimeout, opts.ReadOnly)
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
	n := &Node{f: f, entry: e, statAt: time.Now(), traceName: e.Name}
	if e.Dir {
		n.mode = 0700
	} else {
		n.mode = 0600
	}
	f.nodes.Store(n, true)
	return n
}
func (n *Node) snapshot() anyshare.Entry { n.mu.Lock(); defer n.mu.Unlock(); return n.entry }

func (f *FileSystem) list(ctx context.Context, n *Node, fresh bool) (result []anyshare.Entry, resultErr error) {
	ctx, finish := f.opts.Trace.Begin(ctx, "list directory %q", n.traceName)
	defer func() { finish("entries=%d; %s", len(result), diagnostic.Status(resultErr)) }()
	e := n.snapshot()
	f.listMu.Lock()
	hit, ok := f.listCache[e.ID]
	if !fresh && ok && time.Now().Before(hit.expires) {
		f.listMu.Unlock()
		f.opts.Trace.Printf(ctx, "directory cache hit: entries=%d", len(hit.entries))
		return append([]anyshare.Entry(nil), hit.entries...), nil
	}
	if flight := f.listFlights[e.ID]; flight != nil {
		f.listMu.Unlock()
		f.opts.Trace.Printf(ctx, "waiting for shared directory listing")
		select {
		case <-flight.done:
			return append([]anyshare.Entry(nil), flight.entries...), flight.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	flight := &listFlight{done: make(chan struct{})}
	f.listFlights[e.ID] = flight
	f.listMu.Unlock()
	// Invalidating the namespace detaches in-flight listings. They still finish
	// for their callers, but cannot repopulate a cache from before the mutation.
	defer func() {
		f.listMu.Lock()
		if f.listFlights[e.ID] == flight {
			delete(f.listFlights, e.ID)
			if resultErr == nil {
				f.listCache[e.ID] = cachedList{append([]anyshare.Entry(nil), result...), time.Now().Add(f.opts.TTL)}
			}
		}
		flight.entries, flight.err = result, resultErr
		close(flight.done)
		f.listMu.Unlock()
	}()
	var entries []anyshare.Entry
	var err error
	f.opts.Trace.Printf(ctx, "directory cache miss; fetching remote listing (forced=%t)", fresh)
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
	return entries, nil
}

func (f *FileSystem) listedAt(id string) time.Time {
	f.listMu.Lock()
	defer f.listMu.Unlock()
	if list, ok := f.listCache[id]; ok {
		return list.expires.Add(-f.opts.TTL)
	}
	return time.Time{}
}
func (f *FileSystem) invalidate() {
	f.listMu.Lock()
	f.listCache = map[string]cachedList{}
	f.listFlights = make(map[string]*listFlight)
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
		f.opts.Trace.Printf(context.Background(), "write cache allocation denied: file=%q requested=%s budget=%s used=%s", n.entry.Name, diagnostic.Size(size), diagnostic.Size(f.opts.CacheLimit), diagnostic.Size(f.cacheBytes))
		return syscall.ENOSPC
	}
	f.cacheBytes += delta
	n.charged = size
	return nil
}

func (n *Node) ensureCacheLocked(ctx context.Context, truncate bool) (result error) {
	ctx, finish := n.f.opts.Trace.Begin(ctx, "prepare write cache file=%q truncate=%t", n.entry.Name, truncate)
	defer func() { finish("%s", diagnostic.Status(result)) }()
	if n.cache != nil {
		n.f.opts.Trace.Printf(ctx, "write cache already present; no download")
		return nil
	}
	if !truncate {
		if file, size := n.f.takeCachedFile(n.entry); file != nil {
			n.f.opts.Trace.Printf(ctx, "promoted complete clean read cache; reused=%s; no download", diagnostic.Size(size))
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
		n.f.opts.Trace.Printf(ctx, "downloading existing file into write cache: size=%s", diagnostic.Size(size))
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

func (n *Node) flushLocked(ctx context.Context) (result error) {
	ctx, finish := n.f.opts.Trace.Begin(ctx, "flush file=%q size=%s", n.entry.Name, diagnostic.Size(n.entry.Size))
	defer func() { finish("%s", diagnostic.Status(result)) }()
	if !n.dirty {
		n.f.opts.Trace.Printf(ctx, "file is clean; no upload")
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
	n.f.opts.Trace.Printf(ctx, "uploading pending write: id=%q revision=%q", old.ID, old.Rev)
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
	n.f.opts.Trace.Printf(ctx, "write published: revision=%q; recovery journal removed", entry.Rev)
	return nil
}

func (n *Node) disposeLocked() {
	if n.cache == nil {
		return
	}
	n.cache.Close()
	n.cache = nil
	if !n.dirty {
		n.f.opts.Trace.Printf(context.Background(), "write cache released: file=%q clean=true unlinked=%t size=%s", n.entry.Name, n.unlinked, diagnostic.Size(n.charged))
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

func (f *FileSystem) Close() (result error) {
	_, finish := f.opts.Trace.Begin(context.Background(), "close mount cache")
	defer func() { finish("%s", diagnostic.Status(result)) }()
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
