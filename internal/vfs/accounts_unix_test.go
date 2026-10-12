//go:build linux || darwin

package vfs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestKernelMultiAccountMount(t *testing.T) {
	fixture := newAccountFixture(t)
	point, _ := mountTest(t, fixture.backend, Options{})
	var inodes [2]uint64
	for i, account := range fixture.roots {
		file := filepath.Join(point, account.Name, fixture.users[i].roots[0].Name, "same.txt")
		stat, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		inodes[i] = stat.Sys().(*syscall.Stat_t).Ino
	}
	if inodes[0] == inodes[1] {
		t.Fatal("colliding raw IDs have the same mounted inode")
	}
	exerciseMountedAccounts(t, point, fixture)
	var stat unix.Statfs_t
	if err := unix.Statfs(point, &stat); err != nil {
		t.Fatal(err)
	}
	if stat.Blocks*uint64(stat.Bsize) != 200<<20 || stat.Bavail*uint64(stat.Bsize) != 127<<20 {
		t.Fatal("kernel capacity double-counts shared library", stat)
	}
}

func TestKernelMultiAccountReadOnlyMount(t *testing.T) {
	fixture := newAccountFixture(t)
	point, _ := mountTest(t, fixture.backend, Options{ReadOnly: true})
	file := filepath.Join(point, "alpha", fixture.users[0].roots[0].Name, "same.txt")
	readFile(t, file, []byte("alpha"))
	if err := os.WriteFile(file, []byte("forbidden"), 0600); err == nil {
		t.Fatal("read-only multi-account mount is writable")
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(point, &stat); err != nil || stat.Bavail != 0 || stat.Blocks*uint64(stat.Bsize) != 73<<20 {
		t.Fatal("read-only capacity includes available or duplicated space", stat, err)
	}
}
