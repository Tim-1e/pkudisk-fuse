package vfs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/winfsp/cgofuse/fuse"
	"golang.org/x/sys/windows"
)

func TestWinFspMultiAccountMount(t *testing.T) {
	fixture := newAccountFixture(t)
	core := accountCore(t, fixture)
	adapter := NewWinFsp(context.Background(), core)
	point := mountWinFspTest(t, adapter)
	exerciseMountedAccounts(t, point, fixture)
	wide, err := windows.UTF16PtrFromString(point)
	if err != nil {
		t.Fatal(err)
	}
	var available, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(wide, &available, &total, &free); err != nil || total != 200<<20 || available != 127<<20 || free != available {
		t.Fatalf("mounted total=%d available=%d free=%d: %v", total, available, free, err)
	}
	if code := adapter.Chmod("/alpha", 0700); code != -fuse.EPERM {
		t.Fatal("virtual account permissions can be changed", code)
	}
}

func TestWinFspMultiAccountReadOnlyMount(t *testing.T) {
	fixture := newAccountFixture(t)
	core := accountCore(t, fixture)
	core.opts.ReadOnly = true
	adapter := NewWinFsp(context.Background(), core)
	point := mountWinFspTest(t, adapter)
	file := filepath.Join(point, "alpha", fixture.users[0].roots[0].Name, "same.txt")
	if data, err := os.ReadFile(file); err != nil || string(data) != "alpha" {
		t.Fatal(string(data), err)
	}
	if err := os.WriteFile(file, []byte("forbidden"), 0600); err == nil {
		t.Fatal("read-only multi-account mount is writable")
	}
	var stat fuse.Statfs_t
	if code := adapter.Statfs("/", &stat); code != 0 || stat.Bavail != 0 || stat.Blocks*stat.Bsize != 73<<20 {
		t.Fatal("read-only capacity includes available or duplicated space", stat, code)
	}
}
