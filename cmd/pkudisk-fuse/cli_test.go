package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thezzisu/pkudisk-fuse/internal/anyshare"
	"github.com/thezzisu/pkudisk-fuse/internal/transfer"
	"github.com/thezzisu/pkudisk-fuse/internal/vfs"
)

func testExecute(args ...string) (string, string, error) {
	var out, errOut bytes.Buffer
	err := execute(context.Background(), args, strings.NewReader(""), &out, &errOut)
	return out.String(), errOut.String(), err
}
func TestHelpIsOfflineAndConsistent(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", directory)
	// Even a malformed account store must not prevent help or completion.
	path := filepath.Join(directory, "pkudisk-fuse")
	os.Mkdir(path, 0700)
	os.WriteFile(filepath.Join(path, "accounts.json"), []byte("not JSON"), 0600)
	paths := [][]string{{"mount"}, {"ls"}, {"upload"}, {"download"}, {"recover"}, {"tui"}, {"account"}, {"account", "add"}, {"account", "list"}, {"account", "default"}, {"account", "remove"}, {"version"}, {"completion"}}
	for _, path := range paths {
		t.Run(strings.Join(path, "/"), func(t *testing.T) {
			direct, stderr, err := testExecute(append(append([]string{}, path...), "--help")...)
			if err != nil || stderr != "" || !strings.Contains(direct, "Usage:") {
				t.Fatalf("direct help: %v stderr=%q output=%q", err, stderr, direct)
			}
			indirect, stderr, err := testExecute(append([]string{"help"}, path...)...)
			if err != nil || stderr != "" || indirect != direct {
				t.Fatalf("nested help differs: %v stderr=%q", err, stderr)
			}
		})
	}
	out, stderr, err := testExecute()
	if err != nil || stderr != "" || !strings.Contains(out, "recover") {
		t.Fatal("empty invocation must show help")
	}
	out, stderr, err = testExecute("completion", "zsh")
	if err != nil || stderr != "" || !strings.Contains(out, "#compdef") {
		t.Fatalf("completion: %v", err)
	}
}
func TestUsageFailuresBeforeAuthentication(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, args := range [][]string{
		{"accounts"}, {"roots"}, {"root"}, {"root", "--help"}, {"help", "root"}, {"account", "use", "unused"}, {"account", "delete", "unused"},
		{"help", "missing"}, {"help", "--json"}, {"recover", "ignored"}, {"root", "ignored"},
		{"account", "default"}, {"account", "add", "one", "two"}, {"upload", "only-source"},
		{"upload", "source", "destination", "--remote", "conflict"}, {"download", "remote", "local", "--output", "conflict"},
		{"ls", "--password-stdin", "--password-file", "file"}, {"ls", "--account", "work", "--username", "user"},
		{"download", "remote", "--io-timeout", "invalid"}, {"download", "remote", "--io-timeout", "0s"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			out, _, err := testExecute(args...)
			if exitCode(err) != 2 || !strings.Contains(err.Error(), "--help") || out != "" {
				t.Fatalf("exit=%d out=%q err=%v", exitCode(err), out, err)
			}
		})
	}
}
func TestNonInteractiveCredentialsNeverPrompt(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, args := range [][]string{{"ls", "--non-interactive"}, {"account", "add", "--non-interactive", "--username", "test-user"}} {
		_, _, err := testExecute(args...)
		if exitCode(err) != 1 || !strings.Contains(err.Error(), "provide") {
			t.Fatalf("error=%v", err)
		}
	}
}
func TestRecoverEmptyAndRecords(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing")
	for _, cache := range []string{root, missing} {
		out, _, err := testExecute("recover", "--json", "--cache-dir", cache)
		if err != nil || strings.TrimSpace(out) != "[]" {
			t.Fatalf("empty JSON=%q err=%v", out, err)
		}
		out, _, err = testExecute("recover", "--cache-dir", cache)
		if err != nil || !strings.Contains(out, "No uncommitted") {
			t.Fatal(out, err)
		}
	}
	cache := filepath.Join(root, "mount-test")
	os.Mkdir(cache, 0700)
	os.WriteFile(filepath.Join(cache, "data-file"), []byte("test"), 0600)
	record := vfs.RecoveryRecord{Entry: anyshare.Entry{ID: "gns://library/file", Name: "report.txt", Size: 4, Rev: "rev"}, Parent: "gns://library", Data: "data-file"}
	data, _ := json.Marshal(record)
	os.WriteFile(filepath.Join(cache, "data-file.json"), data, 0600)
	out, _, err := testExecute("recover", "--cache-dir", root)
	if err != nil || !strings.Contains(out, filepath.Join(cache, "data-file")) || !strings.Contains(out, "report.txt") || !strings.Contains(out, "copy") {
		t.Fatal(out, err)
	}
	out, _, err = testExecute("recover", "--cache-dir", root, "--json")
	var records []vfs.RecoveryRecord
	if err != nil || json.Unmarshal([]byte(out), &records) != nil || len(records) != 1 {
		t.Fatal(out, err)
	}
	if records[0].Data != filepath.Join(cache, "data-file") {
		t.Fatal("recover omitted absolute data path")
	}
}
func TestProgressRedirectedProducesNoEscapes(t *testing.T) {
	var out bytes.Buffer
	progress := newCLIProgress("upload", &out)
	for _, phase := range []transfer.Phase{transfer.Transferring, transfer.Finalizing, transfer.Complete} {
		progress.Update(transfer.Progress{Done: 4, Total: 4, Phase: phase})
	}
	progress.Finish(nil)
	if out.Len() != 0 {
		t.Fatal("redirected progress contaminated output")
	}
}
