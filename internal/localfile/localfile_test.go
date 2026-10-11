package localfile

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestPrivateFileAndAtomicInstallation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	file, err := CreateTemp(dir, "credentials-*")
	if err != nil {
		t.Fatal(err)
	}
	source := file.Name()
	if _, err := file.WriteString("private test data"); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	file.Close()
	target := filepath.Join(dir, "saved")
	if err := Install(source, target, false); err != nil {
		t.Fatal(err)
	}
	file, err = OpenPrivate(target, os.O_RDONLY)
	if err != nil {
		t.Fatal("installed file lost private permissions:", err)
	}
	file.Close()
	other, err := CreateTemp(dir, "other-*")
	if err != nil {
		t.Fatal(err)
	}
	other.WriteString("replacement")
	other.Close()
	if err := Install(other.Name(), target, false); !errors.Is(err, os.ErrExist) {
		t.Fatalf("non-overwrite installation: %v", err)
	}
	if err := Install(other.Name(), target, true); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "replacement" {
		t.Fatalf("replacement=%q error=%v", got, err)
	}
}

func TestLockAcrossProcesses(t *testing.T) {
	if path := os.Getenv("PKUDISK_TEST_LOCK_PATH"); path != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
		defer cancel()
		file, err := Lock(ctx, path, true)
		if file != nil {
			file.Close()
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("child acquired a held lock: %v", err)
		}
		return
	}
	dir := filepath.Join(t.TempDir(), "private")
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "lock")
	file, err := Lock(context.Background(), path, true)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestLockAcrossProcesses$")
	command.Env = append(os.Environ(), "PKUDISK_TEST_LOCK_PATH="+path)
	output, err := command.CombinedOutput()
	file.Close()
	if err != nil {
		t.Fatalf("child lock check: %v\n%s", err, output)
	}
	file, err = Lock(context.Background(), path, true)
	if err != nil {
		t.Fatal("lock not released on close:", err)
	}
	file.Close()
}

func TestExpandedHomePaths(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"~/report.txt", `~\report.txt`} {
		if got := ExpandPath(value); got != filepath.Join(home, "report.txt") {
			t.Fatalf("ExpandPath(%q)=%q", value, got)
		}
	}
	if ExpandPath("~") != home || ExpandPath("~another/file") != "~another/file" {
		t.Fatal("home expansion corrupted a path")
	}
}
