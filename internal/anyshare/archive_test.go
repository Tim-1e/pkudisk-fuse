package anyshare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServerArchivePollingAndDownload(t *testing.T) {
	calls := 0
	payload := "ZIP payload"
	var states []ArchiveProgress
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/open-doc/v1/file-download":
			var body struct {
				Name string `json:"name"`
				Doc  []struct {
					ID string `json:"id"`
				} `json:"doc"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if r.Method != "POST" || body.Name != "folder.zip" || len(body.Doc) != 1 || body.Doc[0].ID != "folder-id" {
				t.Error("incorrect archive selector")
			}
			calls++
			progress := 50
			if calls == 2 {
				progress = 100
			}
			json.NewEncoder(w).Encode(map[string]any{"progress": progress, "package_address": "/package", "items": []map[string]string{{"status": "completed"}}})
		case "/package":
			if r.URL.Query().Get("token") == "" || r.Header.Get("Authorization") != "" {
				t.Error("incorrect package authentication")
			}
			io.WriteString(w, payload)
		default:
			t.Error("unexpected request", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, _ := New(server.URL, &testTokenSource{token: "test-token"})
	var dest bytes.Buffer
	n, err := client.DownloadArchive(context.Background(), Entry{ID: "gns://lib/folder-id", Name: "folder", Dir: true}, &dest, func(p ArchiveProgress) { states = append(states, p) })
	if err != nil || calls != 2 || n != int64(len(payload)) || dest.String() != payload {
		t.Fatalf("n=%d calls=%d err=%v", n, calls, err)
	}
	if !states[0].Preparing || states[0].Percent != 50 || states[len(states)-1].Done != n {
		t.Fatal(states)
	}
}
func TestArchiveRejectsIncompleteAndUntrustedResponses(t *testing.T) {
	for _, reply := range []string{
		`{"progress":100,"package_address":"/package","items":[{"status":"failure","error":{"code":403001002}}]}`,
		`{"progress":100,"package_address":"https://another-host.invalid/package?private=secret","items":[]}`,
		`{"progress":100,"package_address":""}`,
		`{"package_address":"/package"}`,
	} {
		t.Run(reply, func(t *testing.T) {
			downloads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/open-doc/v1/file-download" {
					io.WriteString(w, reply)
				} else {
					downloads++
					t.Error("incomplete/untrusted archive downloaded")
				}
			}))
			defer server.Close()
			c, _ := New(server.URL, &testTokenSource{token: "test-token"})
			_, err := c.DownloadArchive(context.Background(), Entry{ID: "gns://lib/dir", Name: "folder", Dir: true}, io.Discard, nil)
			if err == nil || downloads != 0 || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "secret") {
				t.Fatal(err)
			}
		})
	}
	c, _ := New("https://disk.example", &testTokenSource{token: "test-token"})
	if _, err := c.archiveAddress("https://disk.example:443/package"); err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"http://disk.example/package", "https://disk.example:444/package", "https://user@disk.example/package"} {
		if _, err := c.archiveAddress(address); err == nil {
			t.Fatal("insecure origin accepted")
		}
	}
}
func TestArchiveAndExtractionPollingCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "file-unarchiver") {
			io.WriteString(w, `{"progress":0,"status":"extracting"}`)
		} else {
			io.WriteString(w, `{"progress":0}`)
		}
	}))
	defer server.Close()
	c, _ := New(server.URL, &testTokenSource{token: "test-token"})
	ctx, cancel := context.WithCancel(context.Background())
	_, err := c.DownloadArchive(ctx, Entry{ID: "gns://lib/dir", Name: "folder", Dir: true}, io.Discard, func(ArchiveProgress) { cancel() })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	_, err = c.Extract(ctx, Entry{ID: "gns://lib/file", Name: "folder.zip"}, "gns://lib", "folder", func(int) { cancel() })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestRemoteExtractionCompletionAndFailure(t *testing.T) {
	for _, reply := range []string{
		`{"progress":100,"status":"complete","unarchive_path":"gns://lib/output"}`,
		`{"progress":100,"status":"extracting","unarchive_path":"gns://lib/output"}`,
		`{"progress":100,"status":"complete","unarchive_path":"gns://other/output"}`,
		`{"progress":50,"status":"failed"}`,
	} {
		t.Run(reply, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					ID     string `json:"doc_id"`
					Parent string `json:"target_path"`
					Name   string `json:"name"`
					OnDup  int    `json:"ondup"`
				}
				json.NewDecoder(r.Body).Decode(&body)
				if body.ID != "file" || body.Parent != "gns://lib" || body.Name != "output" || body.OnDup != 1 {
					t.Error("incorrect extraction request")
				}
				io.WriteString(w, reply)
			}))
			defer server.Close()
			c, _ := New(server.URL, &testTokenSource{token: "test-token"})
			result, err := c.Extract(context.Background(), Entry{ID: "gns://lib/file", Name: "source.zip"}, "gns://lib", "output", nil)
			wantSuccess := strings.Contains(reply, `"status":"complete"`) && strings.Contains(reply, `"gns://lib/output"`)
			if (err == nil) != wantSuccess {
				t.Fatal(result, err)
			}
			if wantSuccess && (!result.Dir || result.Name != "output") {
				t.Fatal(result)
			}
		})
	}
}
