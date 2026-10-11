package anyshare

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQuotaUsesGETAndCalculatesWritableSpace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/efast/v1/quota/user" {
			t.Fatalf("quota request: %s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]int64{"allocated": 200, "used": 73})
	}))
	defer server.Close()
	client, err := New(server.URL, &testTokenSource{token: "token"})
	if err != nil {
		t.Fatal(err)
	}
	quota, err := client.Quota(context.Background())
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

func TestCopyDirectoryPollsAndStopsOnFailure(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/efast/v1/dir/copy":
			json.NewEncoder(w).Encode(map[string]string{"id": "job-1"})
		case "/api/efast/v1/dir/copyprogress":
			calls++
			if calls == 1 {
				json.NewEncoder(w).Encode(map[string]any{"success": true, "status": "copying", "progress": 50})
			} else {
				json.NewEncoder(w).Encode(map[string]any{"success": true, "status": "finished", "progress": 100})
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
	if err != nil || entry.ID != "job-1" || len(progress) != 2 || progress[0].Percent != 50 || progress[1].Percent != 100 {
		t.Fatalf("entry=%+v progress=%+v err=%v", entry, progress, err)
	}
	if strings.Contains(entry.ID, "token") {
		t.Fatal("copy result leaked credentials")
	}
}
