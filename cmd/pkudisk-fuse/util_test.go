package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thezzisu/pkudisk-fuse/internal/accounts"
	"github.com/thezzisu/pkudisk-fuse/internal/auth"
)

// Exercise command parsing, saved-session restore, API responses and stdout /
// stderr together. The response schema comes from matched PKU searches, including
// integer microsecond timestamps and folder size=-1, rather than an empty page.
func TestUtilCommandsThroughHTTP(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const lib = "gns://00000000000000000000000000000001"
	var refreshes, copies atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/anyshare/oauth2/login/refreshToken" {
			number := refreshes.Add(1)
			http.SetCookie(w, &http.Cookie{Name: "client.oauth2_token", Value: fmt.Sprintf("access-%d", number), Path: "/"})
			http.SetCookie(w, &http.Cookie{Name: "client.oauth2_refresh_token", Value: fmt.Sprintf("refresh-%d", number), Path: "/"})
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer access-") {
			t.Error("API request has no restored authentication")
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/api/efast/v1/quota/doc-lib/" + lib:
			json.NewEncoder(w).Encode(map[string]int64{"allocated": 200, "used": 73})
		case "/api/eacp/v1/perm1/checkall":
			fmt.Fprint(w, `{"allow":["create","modify"]}`)
		case "/api/efast/v1/entry-doc-lib":
			fmt.Fprintf(w, `[{"id":%q,"name":"Library","size":-1,"type":"user_doc_lib"}]`, lib)
		case "/api/efast/v1/folders/" + lib + "/sub_objects":
			fmt.Fprintf(w, `{"dirs":[{"docid":%q,"name":"Reports","size":-1},{"docid":%q,"name":"Backup","size":-1}],"files":[{"docid":%q,"name":"test.txt","size":212}]}`, lib+"/reports", lib+"/backup", lib+"/file")
		case "/api/ecosearch/v1/file-search":
			var query struct {
				Keyword    string   `json:"keyword"`
				Ranges     []string `json:"range"`
				Rows       int      `json:"rows"`
				Dimensions []string `json:"dimension"`
			}
			if err := json.NewDecoder(r.Body).Decode(&query); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			if query.Rows != 25 && query.Rows != 2 {
				t.Error("incorrect search limit", query.Rows)
			}
			if len(query.Ranges) > 0 && query.Ranges[0] != lib+"/reports*" {
				t.Error("incorrect scoped search", query.Ranges)
			}
			if len(query.Dimensions) != 1 || query.Dimensions[0] != "basename" {
				t.Error("search must include files and folders")
			}
			if query.Keyword == "no-match" {
				fmt.Fprint(w, `{"files":[],"hits":0,"next":25}`)
				return
			}
			fmt.Fprintf(w, `{"files":[{"doc_id":%q,"basename":"test","extension":".txt","size":212,"parent_path":%q,"modified_at":1753084919050792,"created_at":1753084919050792},{"doc_id":%q,"basename":"Reports","extension":"","size":-1,"parent_path":%q,"modified_at":1753084919050792}],"hits":2,"next":2}`, lib+"/file", lib, lib+"/reports", lib)
		case "/api/efast/v1/file/copy":
			copies.Add(1)
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["docid"] != lib+"/file" || body["destparent"] != lib+"/backup" {
				t.Error("incorrect copy selectors")
			}
			fmt.Fprintf(w, `{"docid":%q}`, lib+"/backup/copy")
		default:
			t.Error("unexpected API path", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	store, err := accounts.Default()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(accounts.Account{Name: "fixture", Username: "test-user", Password: "fixture-password", Server: server.URL, Session: auth.State{AccessToken: "old", RefreshToken: "old-refresh"}}, true, false); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		out, stderr, err := testExecute(append(args, "--non-interactive")...)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stderr, `Using account "fixture"`) || strings.Contains(out, "Using account") {
			t.Fatal("account announcement mixed with result output")
		}
		return out
	}
	t.Run("library-listing", func(t *testing.T) {
		text := run("ls")
		if !strings.Contains(text, "USED") || !strings.Contains(text, "36.5%") || !strings.Contains(text, "RW") {
			t.Fatal(text)
		}
		var entries []struct {
			ID   string `json:"id"`
			Size int64  `json:"size"`
		}
		if err := json.Unmarshal([]byte(run("ls", "--json")), &entries); err != nil || len(entries) != 1 || entries[0].Size != -1 {
			t.Fatal("ls JSON schema changed", err)
		}
	})
	t.Run("quota", func(t *testing.T) {
		if out := run("util", "quota"); !strings.Contains(out, "127 bytes") {
			t.Fatal(out)
		}
	})
	for _, scope := range []string{"", "/Library/Reports", lib + "/reports"} {
		t.Run("matched-"+scope, func(t *testing.T) {
			args := []string{"util", "search", "test", "--limit", "2"}
			if scope != "" {
				args = append(args, "--in", scope)
			}
			out := run(args...)
			if !strings.Contains(out, "test.txt") || !strings.Contains(out, "Reports") || strings.Contains(out, "EiB") {
				t.Fatal(out)
			}
			var response struct {
				Hits  int `json:"hits"`
				Files []struct {
					Name     string    `json:"name"`
					Size     int64     `json:"size"`
					Dir      bool      `json:"directory"`
					Modified time.Time `json:"modified_at"`
					Path     string    `json:"path"`
				} `json:"files"`
			}
			if err := json.Unmarshal([]byte(run(append(args, "--json")...)), &response); err != nil {
				t.Fatal(err)
			}
			if len(response.Files) != 2 || !response.Files[0].Modified.Equal(time.UnixMicro(1753084919050792)) || !response.Files[1].Dir || response.Files[1].Size != -1 || response.Files[0].Path != lib {
				t.Fatal("matched JSON lost metadata", response)
			}
		})
	}
	t.Run("empty-json", func(t *testing.T) {
		var response map[string]any
		if err := json.Unmarshal([]byte(run("util", "search", "no-match", "--json")), &response); err != nil {
			t.Fatal(err)
		}
		if rows, ok := response["files"].([]any); !ok || len(rows) != 0 {
			t.Fatal("empty results must be an array", response)
		}
	})
	t.Run("copy", func(t *testing.T) {
		out := run("util", "copy", "/Library/test.txt", "/Library/Backup")
		if !strings.Contains(out, "Copied") || copies.Load() != 1 {
			t.Fatal(out)
		}
	})
	if refreshes.Load() < 8 {
		t.Fatal("commands did not exercise session restore")
	}
}
