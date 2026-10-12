package anyshare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestQuotaUsesGETAndCalculatesWritableSpace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/efast/v1/quota/doc-lib/gns://library" {
			t.Fatalf("quota request: %s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]int64{"allocated": 200, "used": 73})
	}))
	defer server.Close()
	client, err := New(server.URL, &testTokenSource{token: "token"})
	if err != nil {
		t.Fatal(err)
	}
	quota, err := client.LibraryQuota(context.Background(), "gns://library")
	if err != nil || quota.Available() != 127 {
		t.Fatalf("quota=%+v err=%v", quota, err)
	}
}

func TestSearchBuildsScopedFilenameQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ecosearch/v1/file-search" {
			t.Fatalf("search path: %s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["keyword"] != "report" || body["rows"] != float64(2) || body["type"] != "doc" {
			t.Fatalf("search body: %#v", body)
		}
		dimensions, ok := body["dimension"].([]any)
		if !ok || len(dimensions) != 1 || dimensions[0] != "basename" {
			t.Fatalf("file/folder search dimensions: %#v", body["dimension"])
		}
		ranges, ok := body["range"].([]any)
		if !ok || len(ranges) != 1 || ranges[0] != "gns://library/folder*" {
			t.Fatalf("scope: %#v", body["range"])
		}
		json.NewEncoder(w).Encode(map[string]any{
			"hits": 1, "next": 2,
			"files": []map[string]any{{"doc_id": "gns://library/file", "basename": "report", "extension": ".txt", "size": 4, "parent_path": "/Library", "modified_at": "2026-10-12T00:00:00Z"}},
		})
	}))
	defer server.Close()
	client, err := New(server.URL, &testTokenSource{token: "token"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Search(context.Background(), "report", "gns://library/folder", 2)
	if err != nil || result.Hits != 1 || result.Next != 2 || len(result.Files) != 1 || result.Files[0].Name != "report.txt" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestSearchDecodesMatchedFileAndFolderResponses(t *testing.T) {
	const micros int64 = 1753084919050792
	for _, stamp := range []string{
		"1753084919050792", `"2025-07-21T08:01:59.050792Z"`, "null", `""`,
	} {
		t.Run(stamp, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"hits":3,"next":3,"files":[
					{"doc_id":"gns://library/a","basename":"中文 report","extension":".txt","size":212,"parent_path":"gns://library/folder","modified_at":%s,"created_at":%s},
					{"doc_id":"gns://library/b","basename":"folder test","extension":"","size":-1,"parent_path":"gns://library","modified_at":%s},
					{"doc_id":"gns://library/c","basename":"README","extension":null,"size":0,"parent_path":"gns://library"}]}`, stamp, stamp, stamp)
			}))
			defer server.Close()
			client, _ := New(server.URL, &testTokenSource{token: "token"})
			result, err := client.Search(context.Background(), "test", "", 25)
			if err != nil || len(result.Files) != 3 {
				t.Fatalf("matched response: %v", err)
			}
			file, folder, empty := result.Files[0], result.Files[1], result.Files[2]
			if file.Name != "中文 report.txt" || file.Size != 212 || file.Dir || file.Path != "gns://library/folder" || folder.Name != "folder test" || !folder.Dir || folder.Size != -1 || empty.Name != "README" || empty.Size != 0 {
				t.Fatalf("incorrect matched results: %+v", result)
			}
			if stamp == "1753084919050792" || strings.HasPrefix(stamp, `"2025`) {
				if !file.Modified.Equal(time.UnixMicro(micros)) || !file.Created.Equal(file.Modified) || !folder.Modified.Equal(file.Modified) {
					t.Fatal("timestamp unit or precision was lost")
				}
			} else if !file.Modified.IsZero() || !file.Created.IsZero() {
				t.Fatal("missing timestamp should stay zero")
			}
		})
	}
}

func TestSearchRejectsInvalidMatchedPayloads(t *testing.T) {
	for _, entry := range []string{
		`{"doc_id":"gns://library/a","basename":"test","size":-2}`,
		`{"doc_id":"gns://library/a","basename":"../test","size":1}`,
		`{"doc_id":"gns://library/a","basename":"test","size":1,"modified_at":true}`,
		`{"doc_id":"gns://library/a","basename":"test","size":1,"created_at":1.5}`,
	} {
		t.Run(entry, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintf(w, `{"files":[%s]}`, entry) }))
			defer server.Close()
			client, _ := New(server.URL, &testTokenSource{token: "token"})
			if _, err := client.Search(context.Background(), "test", "", 25); err == nil {
				t.Fatal("invalid search result accepted")
			}
		})
	}
}

func TestCopyFileReturnsServerDocID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/efast/v1/file/copy" {
			t.Fatalf("copy path: %s", r.URL.Path)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["docid"] != "gns://library/file" || body["destparent"] != "gns://library/dest" {
			t.Fatalf("copy body: %#v", body)
		}
		json.NewEncoder(w).Encode(map[string]string{"docid": "gns://library/dest/file-new", "rev": "2"})
	}))
	defer server.Close()
	client, err := New(server.URL, &testTokenSource{token: "token"})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := client.Copy(context.Background(), Entry{ID: "gns://library/file", Name: "report.txt", Size: 4}, Entry{ID: "gns://library/dest", Dir: true}, nil)
	if err != nil || entry.ID != "gns://library/dest/file-new" || entry.Name != "report.txt" {
		t.Fatalf("entry=%+v err=%v", entry, err)
	}
}

func TestCopyDirectoryWaitsForCompletion(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/efast/v1/dir/copy":
			json.NewEncoder(w).Encode(map[string]string{"id": "job-1", "docid": "gns://library/dest/copied-folder"})
		case "/api/efast/v1/dir/copyprogress":
			calls++
			if calls == 1 {
				json.NewEncoder(w).Encode(map[string]any{"success": true, "status": "copying", "size": 100, "sizecopied": 50})
			} else {
				json.NewEncoder(w).Encode(map[string]any{"success": true, "status": "finished", "size": 100, "sizecopied": 100})
			}
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, &testTokenSource{token: "token"})
	if err != nil {
		t.Fatal(err)
	}
	var progress []CopyProgress
	entry, err := client.Copy(context.Background(), Entry{ID: "gns://library/folder", Name: "folder", Dir: true, Size: -1}, Entry{ID: "gns://library/dest", Dir: true}, func(p CopyProgress) { progress = append(progress, p) })
	if err != nil || entry.ID != "gns://library/dest/copied-folder" || len(progress) != 2 || progress[0].Percent != 50 || progress[1].Percent != 100 {
		t.Fatalf("entry=%+v progress=%+v err=%v", entry, progress, err)
	}
	if strings.Contains(entry.ID, "token") {
		t.Fatal("copy result leaked credentials")
	}
}

func TestCopyDirectoryFailureAndCancellation(t *testing.T) {
	for _, status := range []string{"failed", "unknown", "copying"} {
		t.Run(status, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/efast/v1/dir/copy" {
					fmt.Fprint(w, `{"id":"job","docid":"gns://library/dest/copy"}`)
					return
				}
				fmt.Fprintf(w, `{"success":true,"status":%q,"filecount":2,"filecopied":1}`, status)
			}))
			defer server.Close()
			c, _ := New(server.URL, &testTokenSource{token: "token"})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, err := c.Copy(ctx, Entry{ID: "gns://library/source", Dir: true, Name: "folder", Size: -1}, Entry{ID: "gns://library/dest", Dir: true}, func(CopyProgress) {
				if status == "copying" {
					cancel()
				}
			})
			if err == nil {
				t.Fatal("unfinished or failed copy was reported complete")
			}
			if status == "copying" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}
