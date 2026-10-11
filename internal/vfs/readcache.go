package vfs

import (
	"container/list"
	"context"
	"errors"
	"io"
	"os"
	"syscall"

	"github.com/thezzisu/pkudisk-fuse/internal/anyshare"
)

type cacheKey struct {
	id, rev   string
	size, off int64
}

type cachedFile struct {
	key  cacheKey
	path string
	size int64
	keys []cacheKey
}

type readFlight struct {
	done chan struct{}
	data []byte
	err  error
	off  int64
	keys []cacheKey
}

func fileKey(entry anyshare.Entry, off int64) cacheKey {
	return cacheKey{entry.ID, entry.Rev, entry.Size, off}
}

// A batched range occupies one file and one LRU entry, indexed by every block
// it covers. Promotion transfers its allocation to the writer.
func (f *FileSystem) detachCachedFileLocked(element *list.Element) cachedFile {
	file := element.Value.(cachedFile)
	for _, key := range file.keys {
		delete(f.readFiles, key)
	}
	f.readLRU.Remove(element)
	f.readBytes -= file.size
	return file
}

func (f *FileSystem) blockSize(entry anyshare.Entry) int64 {
	size := f.opts.Read.BlockSize
	if entry.Size <= f.opts.Read.WholeFile && (f.opts.CacheLimit == 0 || entry.Size <= f.opts.CacheLimit) {
		size = max(size, entry.Size)
	}
	if f.opts.CacheLimit > 0 {
		size = min(size, f.opts.CacheLimit)
	}
	return size
}

func (f *FileSystem) acquireRead(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case f.readSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// All clean reads and complete write files share the same disk budget. Only
// clean files enter this LRU; pending writes and recovery data cannot be evicted.
func (f *FileSystem) cacheRoomLocked(size int64) bool {
	if f.opts.CacheLimit > 0 && size > f.opts.CacheLimit-(f.cacheBytes-f.readBytes) {
		return false
	}
	for f.opts.CacheLimit > 0 && size > f.opts.CacheLimit-f.cacheBytes {
		oldest := f.readLRU.Front()
		if oldest == nil {
			return false
		}
		file := oldest.Value.(cachedFile)
		if err := os.Remove(file.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false
		}
		f.detachCachedFileLocked(oldest)
		f.cacheBytes -= file.size
	}
	return true
}

func (f *FileSystem) retainCachedFile(entry anyshare.Entry, path string, size int64) {
	f.budgetMu.Lock()
	defer f.budgetMu.Unlock()
	key := fileKey(entry, -1)
	if old := f.readFiles[key]; old != nil {
		file := f.detachCachedFileLocked(old)
		os.Remove(file.path)
		f.cacheBytes -= file.size
	}
	f.readFiles[key] = f.readLRU.PushBack(cachedFile{key: key, path: path, size: size, keys: []cacheKey{key}})
	f.readBytes += size
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
		f.detachCachedFileLocked(cached)
		return file, data.size
	}
	return nil, 0
}

func (f *FileSystem) clearReadCache() {
	f.budgetMu.Lock()
	defer f.budgetMu.Unlock()
	for f.readLRU.Len() > 0 {
		file := f.detachCachedFileLocked(f.readLRU.Front())
		os.Remove(file.path)
		f.cacheBytes -= file.size
	}
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
		n, err := file.ReadAt(data, off-max(cached.key.off, 0))
		file.Close()
		f.readLRU.MoveToBack(element)
		return n, err, true
	}
	return 0, nil, false
}

func (f *FileSystem) readBlock(ctx context.Context, entry anyshare.Entry, data []byte, off int64, blockSize, fetchSize int64) (int, error) {
	start := off / blockSize * blockSize
	key := fileKey(entry, start)
	f.budgetMu.Lock()
	if n, err, ok := f.cachedReadLocked(entry, data, off, key); ok {
		f.budgetMu.Unlock()
		return n, err
	}
	flight := f.readFlights[key]
	owner := flight == nil
	length := min(blockSize, entry.Size-start)
	if owner {
		flight = &readFlight{done: make(chan struct{}), off: start, keys: []cacheKey{key}}
		f.readFlights[key] = flight
		// Only batch uncached, unclaimed neighbours. Pending writes reduce the
		// available read budget, so speculative data cannot crowd them out.
		if f.opts.CacheLimit > 0 {
			fetchSize = min(fetchSize, f.opts.CacheLimit-(f.cacheBytes-f.readBytes))
		}
		for next := start + length; next < entry.Size; next = start + length {
			size := min(blockSize, entry.Size-next)
			if size > fetchSize-length {
				break
			}
			neighbour := fileKey(entry, next)
			if f.readFiles[neighbour] != nil || f.readFlights[neighbour] != nil {
				break
			}
			flight.keys = append(flight.keys, neighbour)
			f.readFlights[neighbour] = flight
			length += size
		}
	}
	f.budgetMu.Unlock()
	if owner {
		err := f.acquireRead(ctx)
		admitted := err == nil
		var n int
		if err == nil {
			// Allocate only after admission; queued reads hold no fetch buffer.
			flight.data = make([]byte, length)
			n, err = f.backend.ReadAt(ctx, entry, flight.data, start)
		}
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
					f.readBytes += int64(n)
					element := f.readLRU.PushBack(cachedFile{key: key, path: file.Name(), size: int64(n), keys: flight.keys})
					for _, key := range flight.keys {
						f.readFiles[key] = element
					}
				} else {
					os.Remove(file.Name())
				}
			}
		}
		for _, key := range flight.keys {
			delete(f.readFlights, key)
		}
		close(flight.done)
		f.budgetMu.Unlock()
		if admitted {
			<-f.readSlots
		}
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
	return copy(data, flight.data[off-flight.off:]), nil
}

func (f *FileSystem) readAt(ctx context.Context, entry anyshare.Entry, data []byte, off, fetchSize int64) (int, error) {
	if entry.Rev == "" {
		if err := f.acquireRead(ctx); err != nil {
			return 0, err
		}
		defer func() { <-f.readSlots }()
		return f.backend.ReadAt(ctx, entry, data, off)
	}
	blockSize := f.blockSize(entry)
	n := 0
	for n < len(data) {
		position := off + int64(n)
		length := min(int64(len(data)-n), blockSize-position%blockSize)
		// Merge a caller's contiguous demand even before a sequential pattern
		// is established. Read-ahead adds neighbours once sequential access grows.
		requested := int64(len(data)-n) + position%blockSize
		requested += (blockSize - requested%blockSize) % blockSize
		batch := max(fetchSize, min(requested, max(blockSize, f.opts.Read.ReadAhead)))
		count, err := f.readBlock(ctx, entry, data[n:n+int(length)], position, blockSize, batch)
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
	blockSize := n.f.blockSize(entry)
	if off == n.readEnd && n.readRev == entry.Rev {
		n.readSize = min(max(blockSize, n.f.opts.Read.ReadAhead), max(blockSize, n.readSize)*2)
	} else {
		n.readSize = blockSize
	}
	n.readEnd, n.readRev = off+int64(len(data)), entry.Rev
	fetchSize := n.readSize
	n.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, n.f.opts.IOTimeout)
	defer cancel()
	count, err := n.f.readAt(ctx, entry, data, off, fetchSize)
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
