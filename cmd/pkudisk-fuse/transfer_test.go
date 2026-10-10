package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"pkudisk-fuse/internal/anyshare"
)

func TestRemoteParent(t *testing.T) {
	tests := []struct {
		selector string
		parent   string
		name     string
	}{
		{"/Library/report.txt", "Library", "report.txt"},
		{"Library/folder/report.txt", "Library/folder", "report.txt"},
		{"gns://00000000000000000000000000000001/folder/report.txt", "gns://00000000000000000000000000000001/folder", "report.txt"},
	}
	for _, test := range tests {
		parent, name, err := remoteParent(test.selector)
		if err != nil {
			t.Errorf("remoteParent(%q): %v", test.selector, err)
			continue
		}
		if parent != test.parent || name != test.name {
			t.Errorf("remoteParent(%q) = (%q, %q), want (%q, %q)", test.selector, parent, name, test.parent, test.name)
		}
	}
}

func TestRemoteParentRejectsUnsafeDestination(t *testing.T) {
	for _, selector := range []string{"", "/", "Library/..", "Library/", "report.txt"} {
		if _, _, err := remoteParent(selector); err == nil {
			t.Errorf("remoteParent(%q) accepted unsafe destination", selector)
		}
	}
}

type transferTestTokens struct{}

func (transferTestTokens) AccessToken() string                   { return "test-token" }
func (transferTestTokens) Refresh(context.Context, string) error { return nil }

func TestDownloadIsAtomicAndRefusesExistingDestination(t *testing.T) {
	content := []byte("downloaded bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/efast/v1/file/osdownload":
			json.NewEncoder(w).Encode(map[string]any{"authrequest": []string{"GET", serverURL(r, "/object")}})
		case "/object":
			w.Header().Set("Content-Length", stringLength(len(content)))
			_, _ = w.Write(content)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := anyshare.New(server.URL, transferTestTokens{})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "out.bin")
	entry := anyshare.Entry{ID: "gns://root/file", Name: "remote.bin", Size: int64(len(content)), Rev: "rev-1"}
	if err := downloadTo(context.Background(), client, entry, target, false); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(target)
	if err != nil || string(b) != string(content) {
		t.Fatalf("downloaded content = %q, err = %v", b, err)
	}
	if err := os.WriteFile(target, []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := downloadTo(context.Background(), client, entry, target, false); err == nil {
		t.Fatal("existing destination was overwritten without --overwrite")
	}
	b, err = os.ReadFile(target)
	if err != nil || string(b) != "keep me" {
		t.Fatalf("existing destination changed to %q, err = %v", b, err)
	}
	if err := downloadTo(context.Background(), client, entry, target, true); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(target)
	if err != nil || !bytes.Equal(b, content) {
		t.Fatalf("explicit overwrite content = %q, err = %v", b, err)
	}
}

func serverURL(r *http.Request, p string) string {
	// The test server is reachable through the Host header and the request's
	// URL does not retain its scheme, so use the local HTTP origin explicitly.
	return "http://" + r.Host + p
}

func stringLength(n int) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 20)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}
