//go:build linux || darwin

package vfs

import (
	"bytes"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
)

func mountTest(t *testing.T, b Backend, options Options) (string, *FileSystem) {
	t.Helper()
	device, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0)
	if err != nil {
		if os.Getenv("PKUDISK_FUSE_REQUIRE_KERNEL") == "1" {
			t.Fatal(err)
		}
		t.Skip("kernel FUSE unavailable:", err)
	}
	device.Close()
	base := t.TempDir()
	point := filepath.Join(base, "mount")
	if err := os.Mkdir(point, 0700); err != nil {
		t.Fatal(err)
	}
	options.CacheDir = filepath.Join(base, "cache")
	options.Logger = log.New(io.Discard, "", 0)
	filesystem, err := New(b, options)
	if err != nil {
		t.Fatal(err)
	}
	zero := time.Duration(0)
	server, err := fs.Mount(point, filesystem.Root(), &fs.Options{EntryTimeout: &zero, AttrTimeout: &zero, NegativeTimeout: &zero})
	if err != nil {
		filesystem.Close()
		if os.Getenv("PKUDISK_FUSE_REQUIRE_KERNEL") == "1" {
			t.Fatal(err)
		}
		t.Skip("FUSE mount unavailable:", err)
	}
	t.Cleanup(func() {
		if err := server.Unmount(); err != nil {
			t.Error(err)
		}
		server.Wait()
		filesystem.Close()
	})
	return point, filesystem
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func readFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: got %q, want %q", path, got, want)
	}
}

func TestKernelRandomWriteTruncateAndFsync(t *testing.T) {
	b := newMemory()
	point, _ := mountTest(t, b, Options{Root: &b.roots[0]})
	path := filepath.Join(point, "中文.txt")
	writeFile(t, path, []byte("abcdefghij"))
	a, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	r, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err = a.WriteAt([]byte("XYZ"), 3); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 10)
	if _, err = r.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if string(got) != "abcXYZghij" {
		t.Fatal(string(got))
	}
	if err = a.Sync(); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	count := b.uploads
	b.mu.Unlock()
	if err = a.Sync(); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	if b.uploads != count {
		t.Error("clean fsync uploaded a duplicate revision")
	}
	b.mu.Unlock()
	if err = a.Truncate(4); err != nil {
		t.Fatal(err)
	}
	if err = a.Truncate(8); err != nil {
		t.Fatal(err)
	}
	if err = a.Sync(); err != nil {
		t.Fatal(err)
	}
	readFile(t, path, []byte{'a', 'b', 'c', 'X', 0, 0, 0, 0})
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Truncate(path, 2); err != nil {
		t.Fatal(err)
	}
	readFile(t, path, []byte("ab"))
}

func TestKernelAppendConcurrentHandles(t *testing.T) {
	b := newMemory()
	point, _ := mountTest(t, b, Options{Root: &b.roots[0]})
	path := filepath.Join(point, "append.txt")
	writeFile(t, path, nil)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, value := range []byte{'A', 'B'} {
		wg.Add(1)
		go func(value byte) {
			defer wg.Done()
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				errs <- err
				return
			}
			for i := 0; i < 20; i++ {
				if _, err = f.Write([]byte{value}); err != nil {
					errs <- err
					f.Close()
					return
				}
			}
			errs <- f.Close()
		}(value)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 40 || bytes.Count(got, []byte("A")) != 20 || bytes.Count(got, []byte("B")) != 20 {
		t.Fatalf("append lost data: %q", got)
	}
}

func TestKernelReplaceAndOpenUnlink(t *testing.T) {
	b := newMemory()
	point, _ := mountTest(t, b, Options{Root: &b.roots[0]})
	source, target := filepath.Join(point, "source"), filepath.Join(point, "target")
	writeFile(t, source, []byte("new data"))
	writeFile(t, target, []byte("old data"))
	old, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if err = os.Rename(source, target); err != nil {
		t.Fatal(err)
	}
	readFile(t, target, []byte("new data"))
	got, err := io.ReadAll(old)
	if err != nil || string(got) != "old data" {
		t.Fatalf("replaced open descriptor: %q, %v", got, err)
	}
	open, err := os.OpenFile(target, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if _, err = open.WriteAt([]byte("orphan"), 0); err != nil {
		t.Fatal(err)
	}
	if err = open.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("unlinked file was resurrected:", err)
	}
}

func TestKernelMoveDirectoryAndRollback(t *testing.T) {
	b := newMemory()
	point, _ := mountTest(t, b, Options{Root: &b.roots[0]})
	a, c := filepath.Join(point, "a"), filepath.Join(point, "b")
	if err := os.Mkdir(a, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(c, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(a, "first")
	writeFile(t, path, []byte("content"))
	if err := os.Rename(path, filepath.Join(c, "different")); err != nil {
		t.Fatal(err)
	}
	readFile(t, filepath.Join(c, "different"), []byte("content"))
	open, err := os.OpenFile(filepath.Join(c, "different"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(c, filepath.Join(a, "moved")); err != nil {
		t.Fatal(err)
	}
	if _, err = open.WriteAt([]byte("C"), 0); err != nil {
		t.Fatal(err)
	}
	if err = open.Close(); err != nil {
		t.Fatal(err)
	}
	readFile(t, filepath.Join(a, "moved", "different"), []byte("Content"))
	writeFile(t, filepath.Join(a, "original"), []byte("rollback"))
	b.mu.Lock()
	b.failMove = true
	b.mu.Unlock()
	if err = os.Rename(filepath.Join(a, "original"), filepath.Join(a, "moved", "new")); err == nil {
		t.Fatal("move failure hidden")
	}
	b.mu.Lock()
	b.failMove = false
	b.mu.Unlock()
	readFile(t, filepath.Join(a, "original"), []byte("rollback"))
	if err = os.Remove(a); !errors.Is(err, syscall.ENOTEMPTY) {
		t.Fatal("nonempty rmdir:", err)
	}
}

func TestKernelUploadFailureRetainsRecoverableData(t *testing.T) {
	b := newMemory()
	point, filesystem := mountTest(t, b, Options{Root: &b.roots[0]})
	path := filepath.Join(point, "retained")
	writeFile(t, path, []byte("before"))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	b.failUpload = true
	b.mu.Unlock()
	if _, err = f.Write([]byte("unsent data")); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err == nil {
		t.Fatal("close acknowledged a failed upload")
	}
	records, err := RecoveryFiles(filesystem.opts.CacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatal("missing recovery journal", len(records))
	}
	readFile(t, records[0].Data, []byte("unsent data"))
	b.mu.Lock()
	b.failUpload = false
	b.mu.Unlock()
	retry, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = retry.Close(); err != nil {
		t.Fatal(err)
	}
	readFile(t, path, []byte("unsent data"))
	records, err = RecoveryFiles(filesystem.opts.CacheDir)
	if err != nil || len(records) != 0 {
		t.Fatal("committed write still journaled", records, err)
	}
}

func TestKernelConflictAndCacheBudget(t *testing.T) {
	b := newMemory()
	point, filesystem := mountTest(t, b, Options{Root: &b.roots[0], CacheLimit: 32})
	path := filepath.Join(point, "conflict")
	writeFile(t, path, []byte("before"))
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteAt([]byte("ours"), 0); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	for id, e := range b.entries {
		if e.Name == "conflict" {
			e.Rev = "changed-elsewhere"
			b.entries[id] = e
			b.data[id] = []byte("before")
		}
	}
	b.mu.Unlock()
	if err = f.Sync(); !errors.Is(err, syscall.EBUSY) {
		t.Fatal("concurrent edit was not rejected:", err)
	}
	if _, err = f.WriteAt(bytes.Repeat([]byte{'x'}, 40), 0); !errors.Is(err, syscall.ENOSPC) {
		t.Fatal("cache limit ignored:", err)
	}
	f.Close()
	records, err := RecoveryFiles(filesystem.opts.CacheDir)
	if err != nil || len(records) != 1 {
		t.Fatal(records, err)
	}
	readFile(t, records[0].Data, []byte("oursre"))
}

func TestKernelMountAllAndReadOnly(t *testing.T) {
	b := newMemory()
	point, _ := mountTest(t, b, Options{})
	entries, err := os.ReadDir(point)
	if err != nil || len(entries) != 1 || entries[0].Name() != "测试文档库" {
		t.Fatal(entries, err)
	}
	if err = os.WriteFile(filepath.Join(point, "outside-library"), nil, 0600); err == nil {
		t.Fatal("created file above libraries")
	}
	readOnly, _ := mountTest(t, b, Options{Root: &b.roots[0], ReadOnly: true})
	if err = os.WriteFile(filepath.Join(readOnly, "forbidden"), nil, 0600); !errors.Is(err, syscall.EROFS) {
		t.Fatal("read-only mount accepted writes:", err)
	}
}
