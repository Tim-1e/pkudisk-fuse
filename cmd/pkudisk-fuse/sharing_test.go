package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thezzisu/pkudisk-fuse/internal/accounts"
	"github.com/thezzisu/pkudisk-fuse/internal/anyshare"
	"github.com/thezzisu/pkudisk-fuse/internal/auth"
)

func TestSharingCLIThroughAuthenticatedHTTP(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const library = "gns://library"
	const file = library + "/file"
	inner, external, granted, mutations := false, false, false, 0
	refreshes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/anyshare/oauth2/login/refreshToken" {
			refreshes++
			http.SetCookie(w, &http.Cookie{Name: "client.oauth2_token", Value: fmt.Sprintf("access-fixture-%d", refreshes), Path: "/"})
			http.SetCookie(w, &http.Cookie{Name: "client.oauth2_refresh_token", Value: "refresh-fixture", Path: "/"})
			return
		}
		if r.Header.Get("Authorization") != fmt.Sprintf("Bearer access-fixture-%d", refreshes) {
			t.Error("sharing call used an unrestored session")
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/api/efast/v1/entry-doc-lib":
			fmt.Fprintf(w, `[{"id":%q,"name":"Library","size":-1,"type":"user_doc_lib"}]`, library)
		case "/api/efast/v1/folders/" + library + "/sub_objects":
			fmt.Fprintf(w, `{"dirs":[],"files":[{"docid":%q,"name":"报告.txt","size":4,"rev":"1"}]}`, file)
		case "/api/doc-share/v1/sharing-configuration-scope/user", "/api/doc-share/v1/sharing-configuration-scope/anyone":
			fmt.Fprint(w, `{"validity_period":{"default":-1,"maximum":-1},"perm":{"allow":["display","preview","download"],"default":["display","preview","download"]},"access_times":{"default":-1,"maximum":-1},"must_set_password":false,"verify_mobile":{"enable":false,"force":false}}`)
		case "/api/eacp/v1/department/search":
			fmt.Fprint(w, `{"userinfos":[{"userid":"recipient","name":"李同学","account":"recipient-account","deppath":"学院"}],"depinfos":[]}`)
		case "/api/shared-link/v1/document/file/" + file:
			if r.URL.Query().Get("type") == "realname" && inner {
				fmt.Fprint(w, `[{"id":"inner-1","type":"realname"}]`)
			} else if r.URL.Query().Get("type") == "anonymous" && external {
				fmt.Fprint(w, `[{"id":"outer-1","type":"anonymous","title":"报告.txt","password":"Code1234","expires_at":"2030-01-01T00:00:00Z","limited_times":3,"item":{"allow":["display","download"]}}]`)
			} else {
				fmt.Fprint(w, `[]`)
			}
		case "/api/shared-link/v1/document/realname":
			inner = true
			mutations++
			fmt.Fprint(w, `{"id":"inner-1"}`)
		case "/api/shared-link/v1/document/anonymous":
			external = true
			mutations++
			fmt.Fprint(w, `{"id":"outer-1"}`)
		case "/api/shared-link/v1/document/anonymous/outer-1":
			if r.Method != "DELETE" {
				t.Error("revoke link is not DELETE")
			}
			external = false
			mutations++
			w.WriteHeader(204)
		case "/api/doc-share/v1/doc-perm":
			granted = true
			mutations++
			var body struct {
				DocID   string `json:"doc_id"`
				Configs []struct{ Accessor anyshare.Recipient }
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.DocID != file || len(body.Configs) != 1 || body.Configs[0].Accessor.ID != "recipient" {
				t.Error("wrong selected recipient")
			}
		case "/api/eacp/v1/perm2/get":
			if granted {
				fmt.Fprint(w, `{"inherit":true,"perminfos":[{"accessorid":"recipient","accessortype":"user","accessorname":"李同学","allow":["display","download"],"deny":[],"endtime":-1,"inheritdocid":""}]}`)
			} else {
				fmt.Fprint(w, `{"inherit":true,"perminfos":[]}`)
			}
		case "/api/eacp/v1/perm2/set":
			granted = false
			mutations++
			fmt.Fprint(w, `{"result":0}`)
		default:
			t.Error("unexpected operation / notification", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	store, err := accounts.Default()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(accounts.Account{Name: "fixture", Username: "test", Password: "private-password", Server: server.URL, Session: auth.State{RefreshToken: "refresh-fixture"}}, true, false); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		args = append(args, "-a", "fixture", "--non-interactive")
		out, stderr, err := testExecute(args...)
		if err != nil || !strings.Contains(stderr, `Using account "fixture"`) || strings.Contains(out, "Using account") {
			t.Fatal(args, out, stderr, err)
		}
		return out
	}
	var recipients []anyshare.Recipient
	if out := run("share", "search", "李", "-j"); json.Unmarshal([]byte(out), &recipients) != nil || len(recipients) != 1 || recipients[0].ID != "recipient" {
		t.Fatal(out)
	}
	if out := run("share", "link", "/Library/报告.txt"); !strings.Contains(out, "/anyshare/zh-cn/link/inner-1?") || granted {
		t.Fatal("navigation link granted access", out)
	}
	run("share", "add", "/Library/报告.txt", "--user", "recipient", "--permission", "download", "--expires-in", "1h")
	if out := run("share", "ls", file); !strings.Contains(out, "李同学") || !strings.Contains(out, "direct") || !granted {
		t.Fatal(out)
	}
	run("share", "remove", file, "--user", "recipient")
	if granted {
		t.Fatal("grant not revoked")
	}
	var links []anyshare.ShareLink
	if out := run("link", "add", file, "--code", "Code1234", "--expires-in", "1h", "--limit", "3", "-j"); json.Unmarshal([]byte(out), &links) != nil || len(links) != 1 || links[0].Code != "Code1234" {
		t.Fatal(out)
	}
	if out := run("link", "ls", file); !strings.Contains(out, "Code1234") || !strings.Contains(out, "outer-1") {
		t.Fatal(out)
	}
	run("link", "remove", "outer-1")
	if out := run("link", "ls", file, "-j"); strings.TrimSpace(out) != "[]" {
		t.Fatal("empty link listing changed JSON shape", out)
	}
	if mutations != 5 {
		t.Fatal("unexpected extra mutations", mutations)
	}
}

func TestSharingRequiredInputsAndHelpStayOffline(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, args := range [][]string{
		{"share", "add"}, {"link", "add"},
		{"share", "add", "path", "--non-interactive"}, {"share", "remove", "path"},
		{"link", "remove", "--non-interactive"},
		{"share", "search", "name", "--limit", "0"},
		{"link", "add", "path", "--limit", "0"},
		{"link", "add", "path", "--expires-in", "-1h"},
		{"link", "add", "path", "--code", "abc"},
		{"share", "add", "path", "--user", "u", "--permission", "owner"},
		{"share", "add", "path", "--interactive"},
		{"link", "add", "path", "-i"},
		{"share", "list", "path"}, {"links"}, {"shares"},
	} {
		out, stderr, err := testExecute(args...)
		if exitCode(err) != 2 || out != "" || stderr != "" {
			t.Fatal("missing/invalid input reached authentication or prompted", args, out, stderr, err)
		}
	}
	for _, group := range []string{"share", "link"} {
		command := newShareCommand()
		if group == "link" {
			command = newLinkCommand()
		}
		for _, sub := range command.Commands() {
			out, stderr, err := testExecute(group, sub.Name(), "-h")
			if err != nil || stderr != "" || !strings.Contains(out, "Examples:") || strings.Contains(out, "--interactive") || !strings.Contains(out, "--non-interactive") {
				t.Fatal("offline help or interaction policy inconsistent", out, stderr, err)
			}
		}
	}
}
