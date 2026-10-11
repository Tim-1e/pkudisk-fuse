package vfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/winfsp/cgofuse/fuse"
	"golang.org/x/sys/windows"
)

func testWinFsp(t *testing.T, budget int64) (*WinFsp, *memoryBackend) {
	t.Helper()
	backend := newMemory()
	core, err := New(backend, Options{Root: &backend.roots[0], CacheDir: filepath.Join(t.TempDir(), "private"), CacheLimit: budget, IOTimeout: time.Second, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { core.Close() })
	return NewWinFsp(context.Background(), core), backend
}

func TestWinFspRandomWritesTruncateAndAppend(t *testing.T) {
	adapter, backend := testWinFsp(t, 0)
	code, handle := adapter.Create("/report.txt", fuse.O_RDWR, 0600)
	if code != 0 {
		t.Fatal(code)
	}
	if n := adapter.Write("", []byte("abcdefgh"), 0, handle); n != 8 {
		t.Fatal(n)
	}
	if n := adapter.Write("", []byte("XY"), 2, handle); n != 2 {
		t.Fatal(n)
	}
	if code := adapter.Truncate("", 6, handle); code != 0 {
		t.Fatal(code)
	}
	if code := adapter.Fsync("", false, handle); code != 0 {
		t.Fatal(code)
	}
	adapter.Release("", handle)
	code, handle = adapter.Open("/report.txt", fuse.O_RDWR|fuse.O_APPEND)
	if code != 0 || adapter.Write("", []byte("!"), 0, handle) != 1 || adapter.Flush("", handle) != 0 {
		t.Fatal("append failed", code)
	}
	got := make([]byte, 7)
	if n := adapter.Read("", got, 0, handle); n != 7 || string(got) != "abXYef!" {
		t.Fatalf("read=%q n=%d", got, n)
	}
	adapter.Release("", handle)
	if backend.uploads != 3 {
		t.Fatalf("published %d uploads, want create and two writes", backend.uploads)
	}
}

func TestWinFspFailuresRetainRecoveryAndRespectBudget(t *testing.T) {
	adapter, backend := testWinFsp(t, 16)
	code, handle := adapter.Create("/retained.bin", fuse.O_RDWR, 0600)
	if code != 0 || adapter.Write("", []byte("unsent data"), 0, handle) != 11 {
		t.Fatal("prepare failed", code)
	}
	if got := adapter.Write("", []byte("too large"), 16, handle); got != -fuse.ENOSPC {
		t.Fatalf("cache limit returned %d", got)
	}
	backend.failUpload = true
	if adapter.Flush("", handle) == 0 {
		t.Fatal("failed upload acknowledged")
	}
	adapter.Release("", handle)
	records, err := RecoveryFiles(adapter.core.cacheDir)
	if err != nil || len(records) != 1 {
		t.Fatalf("recovery records=%d error=%v", len(records), err)
	}
	got, err := os.ReadFile(records[0].Data)
	if err != nil || string(got) != "unsent data" {
		t.Fatal("failed upload data lost", err)
	}
	backend.failUpload = false
	code, handle = adapter.Open("/retained.bin", fuse.O_RDWR)
	if code != 0 || adapter.Flush("", handle) != 0 {
		t.Fatal("recovery retry failed", code)
	}
	adapter.Release("", handle)
	records, err = RecoveryFiles(adapter.core.cacheDir)
	if err != nil || len(records) != 0 {
		t.Fatal("published data remained in recovery", err)
	}
}

func TestWinFspRenameOpenFileAndRevisionConflict(t *testing.T) {
	adapter, backend := testWinFsp(t, 0)
	code, handle := adapter.Create("/source.txt", fuse.O_RDWR, 0600)
	if code != 0 || adapter.Write("", []byte("contents"), 0, handle) != 8 || adapter.Flush("", handle) != 0 {
		t.Fatal("prepare source failed", code)
	}
	if adapter.Mkdir("/folder", 0700) != 0 || adapter.Rename("/source.txt", "/folder/moved.txt") != 0 {
		t.Fatal("rename failed")
	}
	got := make([]byte, 8)
	if adapter.Read("", got, 0, handle) != 8 || string(got) != "contents" {
		t.Fatal("open handle did not survive move")
	}
	node := adapter.handle(handle).node
	backend.mu.Lock()
	entry := backend.entries[node.entry.ID]
	entry.Rev = "external-revision"
	backend.entries[entry.ID] = entry
	backend.mu.Unlock()
	if adapter.Write("", []byte("modified"), 0, handle) != 8 || adapter.Flush("", handle) != -fuse.EBUSY {
		t.Fatal("external revision change was overwritten")
	}
	adapter.Release("", handle)
}

func mountWinFspTest(t *testing.T, adapter *WinFsp) string {
	t.Helper()
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		t.Fatal(err)
	}
	point := ""
	for letter := byte('P'); letter <= 'Z'; letter++ {
		if mask&(1<<uint(letter-'A')) == 0 {
			point = string(letter) + ":"
			break
		}
	}
	if point == "" {
		t.Fatal("no free drive for WinFsp test")
	}
	host := fuse.NewFileSystemHost(adapter)
	host.SetCapOpenTrunc(true)
	host.SetCapReaddirPlus(true)
	done := make(chan bool, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- false
			}
		}()
		done <- host.Mount(point, WinFspMountOptions())
	}()
	select {
	case ok := <-done:
		if os.Getenv("PKUDISK_FUSE_REQUIRE_KERNEL") == "1" {
			t.Fatalf("WinFsp mount ended before initialization: %v", ok)
		}
		t.Skip("WinFsp driver unavailable")
	case <-adapter.Ready:
	case <-time.After(20 * time.Second):
		t.Fatal("WinFsp initialization timed out")
	}
	t.Cleanup(func() {
		if !host.Unmount() {
			t.Error("WinFsp unmount failed")
		}
		select {
		case ok := <-done:
			if !ok {
				t.Error("WinFsp mount failed")
			}
		case <-time.After(20 * time.Second):
			t.Error("WinFsp did not exit")
		}
	})
	root := point + `\`
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(root); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("WinFsp drive never appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return root
}

func TestWinFspMountedRoundTrip(t *testing.T) {
	adapter, _ := testWinFsp(t, 0)
	root := mountWinFspTest(t, adapter)
	dir := filepath.Join(root, "目录")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "报告.txt")
	file, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("native WinFsp mount round trip")
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	got, err := os.ReadFile(name)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("native read=%q error=%v", got, err)
	}
	destination := filepath.Join(dir, "renamed.txt")
	if err := os.Rename(name, destination); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(destination); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
}

func TestWinFspMountedPhotoDecoderAndMemoryMapping(t *testing.T) {
	core, probe := readCacheTest(t, 16<<20)
	picture := image.NewNRGBA(image.Rect(0, 0, 1024, 768))
	for y := 0; y < 768; y++ {
		for x := 0; x < 1024; x++ {
			picture.SetNRGBA(x, y, color.NRGBA{R: byte(x*17 + y*11), G: byte(x*7 + y*19), B: byte(x*31 + y*3), A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, picture, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	data := encoded.Bytes()
	// JPEG decoders accept padding after the end marker. Match the reported
	// 5 MiB file size while file mapping exercises both cached ranges.
	data = append(data, make([]byte, (5<<20)-len(data))...)
	seedFile(t, probe.memoryBackend, "照片.jpg", data)
	adapter := NewWinFsp(context.Background(), core)
	root := mountWinFspTest(t, adapter)
	name := filepath.Join(root, "照片.jpg")
	file, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoded, err := jpeg.Decode(file)
	if err != nil || decoded.Bounds() != picture.Bounds() {
		t.Fatal("native mounted JPEG decode failed", err)
	}
	for i := 0; i < 100; i++ {
		off := int64((i * 1193) % (len(data) - 32))
		got := make([]byte, 32)
		if _, err := file.ReadAt(got, off); err != nil || !bytes.Equal(got, data[off:off+32]) {
			t.Fatal("decoder-style random read failed", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWinFspPhotoMmapClient$")
	command.Env = append(os.Environ(), "PKUDISK_TEST_MMAP="+name, fmt.Sprintf("PKUDISK_TEST_MMAP_HASH=%x", sha256.Sum256(data)))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Windows mmap client: %v\n%s", err, output)
	}
	command = exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `Add-Type -AssemblyName System.Drawing; $image = [System.Drawing.Image]::FromFile($env:PKUDISK_TEST_PHOTO); try { if ($image.Width -ne 1024 -or $image.Height -ne 768) { throw 'incorrect image dimensions' }; $null = ([System.Drawing.Bitmap]$image).GetPixel(511, 383) } finally { $image.Dispose() }`)
	command.Env = append(os.Environ(), "PKUDISK_TEST_PHOTO="+name)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Windows GDI+ image decoder: %v\n%s", err, output)
	}
	if probe.reads.Load() != 2 {
		t.Fatalf("5 MiB image decode, random reads, mmap and reopen fetched %d remote ranges, want two", probe.reads.Load())
	}
}

func TestWinFspPhotoMmapClient(t *testing.T) {
	path := os.Getenv("PKUDISK_TEST_MMAP")
	if path == "" {
		t.Skip("mmap client helper")
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	mapping, err := windows.CreateFileMapping(windows.Handle(file.Fd()), nil, windows.PAGE_READONLY, 0, 0, nil)
	if err != nil {
		t.Fatal("mapped file creation failed", err)
	}
	defer windows.CloseHandle(mapping)
	address, err := windows.MapViewOfFile(mapping, windows.FILE_MAP_READ, 0, 0, uintptr(stat.Size()))
	if err != nil {
		t.Fatal("mapped view failed", err)
	}
	defer windows.UnmapViewOfFile(address)
	mapped := make([]byte, int(stat.Size()))
	var read uintptr
	if err := windows.ReadProcessMemory(windows.CurrentProcess(), address, &mapped[0], uintptr(len(mapped)), &read); err != nil || read != uintptr(len(mapped)) {
		t.Fatal("mapped image read failed", read, err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(mapped)) != os.Getenv("PKUDISK_TEST_MMAP_HASH") {
		t.Fatal("mapped image bytes differ")
	}
}
