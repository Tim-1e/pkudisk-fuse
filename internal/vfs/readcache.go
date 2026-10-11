package vfs

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"

	"github.com/thezzisu/pkudisk-fuse/internal/anyshare"
)

// Fetch larger ranges once for small decoder reads and sequential access.
// Files smaller than a block are fetched whole; large files stay bounded.
const readBlockSize int64 = 4 << 20

type cacheKey struct {
	id, rev   string
	size, off int64
}

type cachedFile struct {
	key  cacheKey
	path string
	size int64
}

type readFlight struct {
	done chan struct{}
	data []byte
	err  error
}

func fileKey(entry anyshare.Entry, off int64) cacheKey {
	return cacheKey{entry.ID, entry.Rev, entry.Size, off}
}

// All clean reads and complete write files share the same disk budget. Only
// clean files enter this LRU; pending writes and recovery data cannot be evicted.
func (f *FileSystem) cacheRoomLocked(size int64) bool {
	for f.opts.CacheLimit > 0 && size > f.opts.CacheLimit-f.cacheBytes {
		oldest := f.readLRU.Front()
		if oldest == nil {
			return false
		}
		file := oldest.Value.(cachedFile)
		if err := os.Remove(file.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false
		}
		f.readLRU.Remove(oldest)
		delete(f.readFiles, file.key)
		f.cacheBytes -= file.size
	}
	return true
}

func (f *FileSystem) retainCachedFile(entry anyshare.Entry, path string, size int64) {
	f.budgetMu.Lock()
	defer f.budgetMu.Unlock()
	key := fileKey(entry, -1)
	if old := f.readFiles[key]; old != nil {
		file := old.Value.(cachedFile)
		os.Remove(file.path)
		f.cacheBytes -= file.size
		f.readLRU.Remove(old)
	}
	f.readFiles[key] = f.readLRU.PushBack(cachedFile{key, path, size})
}

// Promote a complete clean read or a previously flushed write into the mutable
// cache without downloading or allocating the same bytes a second time.
func (f *FileSystem) takeCachedFile(entry anyshare.Entry) (*os.File, int64) {
	f.budgetMu.Lock()
	defer f.budgetMu.Unlock()
	for _, key := range []cacheKey{fileKey(entry, -1), fileKey(entry, 0)} {
		cached := f.readFiles[key]
		if cached == nil {
			continue
		}
		data := cached.Value.(cachedFile)
		if data.size != entry.Size {
			continue
		}
		file, err := os.OpenFile(data.path, os.O_RDWR, 0600)
		if err != nil {
			continue
		}
		delete(f.readFiles, key)
		f.readLRU.Remove(cached)
		return file, data.size
	}
	return nil, 0
}

func (f *FileSystem) clearReadCache() {
	f.budgetMu.Lock()
	defer f.budgetMu.Unlock()
	for key, element := range f.readFiles {
		file := element.Value.(cachedFile)
		os.Remove(file.path)
		f.cacheBytes -= file.size
		delete(f.readFiles, key)
	}
	f.readLRU.Init()
}

func (f *FileSystem) cachedReadLocked(entry anyshare.Entry, data []byte, off int64, key cacheKey) (int, error, bool) {
	for _, candidate := range []cacheKey{fileKey(entry, -1), key} {
		element := f.readFiles[candidate]
		if element == nil {
			continue
		}
		cached := element.Value.(cachedFile)
		file, err := os.Open(cached.path)
		if err != nil {
			return 0, err, true
		}
		n, err := file.ReadAt(data, off-max(candidate.off, 0))
		file.Close()
		f.readLRU.MoveToBack(element)
		return n, err, true
	}
	return 0, nil, false
}

func (f *FileSystem) readBlock(ctx context.Context, entry anyshare.Entry, data []byte, off int64, blockSize int64) (int, error) {
	start := off / blockSize * blockSize
	key := fileKey(entry, start)
	f.budgetMu.Lock()
	if n, err, ok := f.cachedReadLocked(entry, data, off, key); ok {
		f.budgetMu.Unlock()
		return n, err
	}
	flight := f.readFlights[key]
	owner := flight == nil
	if owner {
		flight = &readFlight{done: make(chan struct{})}
		f.readFlights[key] = flight
	}
	f.budgetMu.Unlock()
	if owner {
		flight.data = make([]byte, min(blockSize, entry.Size-start))
		n, err := f.backend.ReadAt(ctx, entry, flight.data, start)
		if err == io.EOF && n == len(flight.data) {
			err = nil
		}
		if err == nil && n != len(flight.data) {
			err = io.ErrUnexpectedEOF
		}
		flight.err = err
		f.budgetMu.Lock()
		if err == nil && f.cacheRoomLocked(int64(n)) {
			file, createErr := os.CreateTemp(f.cacheDir, "read-*.data")
			if createErr == nil {
				_, writeErr := file.Write(flight.data)
				closeErr := file.Close()
				if writeErr == nil && closeErr == nil {
					f.cacheBytes += int64(n)
					f.readFiles[key] = f.readLRU.PushBack(cachedFile{key, file.Name(), int64(n)})
				} else {
					os.Remove(file.Name())
				}
			}
		}
		delete(f.readFlights, key)
		close(flight.done)
		f.budgetMu.Unlock()
	} else {
		select {
		case <-flight.done:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	if flight.err != nil {
		return 0, flight.err
	}
	return copy(data, flight.data[off-start:]), nil
}

func (f *FileSystem) readAt(ctx context.Context, entry anyshare.Entry, data []byte, off int64) (int, error) {
	if entry.Rev == "" {
		return f.backend.ReadAt(ctx, entry, data, off)
	}
	blockSize := readBlockSize
	if f.opts.CacheLimit > 0 {
		blockSize = min(blockSize, f.opts.CacheLimit)
	}
	n := 0
	for n < len(data) {
		position := off + int64(n)
		length := min(int64(len(data)-n), blockSize-position%blockSize)
		count, err := f.readBlock(ctx, entry, data[n:n+int(length)], position, blockSize)
		n += count
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// Remote I/O does not hold the node lock, allowing stat and cached reads to
// proceed while another reader is waiting on the server.
func (n *Node) readAt(ctx context.Context, data []byte, off int64) (int, error) {
	n.mu.Lock()
	if off < 0 {
		n.mu.Unlock()
		return 0, syscall.EINVAL
	}
	if off >= n.entry.Size || len(data) == 0 {
		n.mu.Unlock()
		return 0, nil
	}
	data = data[:min(int64(len(data)), n.entry.Size-off)]
	entry := n.entry
	if n.cache != nil {
		count, err := n.cache.ReadAt(data, off)
		n.mu.Unlock()
		if count != len(data) && (err == nil || err == io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return count, err
	}
	n.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, n.f.opts.IOTimeout)
	defer cancel()
	count, err := n.f.readAt(ctx, entry, data, off)
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.cache != nil {
		// A writer may have prepared or changed the local file during the fetch.
		if off >= n.entry.Size {
			return 0, nil
		}
		data = data[:min(int64(len(data)), n.entry.Size-off)]
		count, err = n.cache.ReadAt(data, off)
	}
	if count != len(data) && (err == nil || err == io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return count, err
}
