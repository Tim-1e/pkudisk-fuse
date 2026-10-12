package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thezzisu/pkudisk-fuse/internal/accounts"
	"github.com/thezzisu/pkudisk-fuse/internal/anyshare"
	"github.com/thezzisu/pkudisk-fuse/internal/auth"
)

func TestAllAccountsFlagScopeAndConflicts(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, flag := range []string{"account", "root", "server", "username", "password-file", "credentials-file", "password-stdin", "no-save"} {
		args := []string{"mount", "unused", "--all-accounts", "--" + flag}
		if flag != "password-stdin" && flag != "no-save" {
			args = append(args, "value")
		}
		out, stderr, err := testExecute(args...)
		if exitCode(err) != 2 || !strings.Contains(err.Error(), "--all-accounts cannot be combined") || out != "" || stderr != "" {
			t.Fatal("invalid selection reached authentication or driver startup", args, out, stderr, err)
		}
	}
	for _, args := range [][]string{
		{"ls", "--all-accounts"}, {"tui", "--all-accounts"},
		{"upload", "local", "remote", "--all-accounts"}, {"download", "remote", "--all-accounts"},
		{"account", "ls", "--all-accounts"}, {"quota", "--all-accounts"},
	} {
		if _, _, err := testExecute(args...); exitCode(err) != 2 {
			t.Fatal("multi-account flag leaked into another command", args, err)
		}
	}
	out, stderr, err := testExecute("mount", "--all-accounts", "-h")
	if err != nil || stderr != "" || !strings.Contains(out, "--all-accounts") || !strings.Contains(out, "Cross-account moves") {
		t.Fatal("multi-account help is incomplete or went online", out, stderr, err)
	}
	if _, err := prepareMount(context.Background(), authFlags{nonInteractive: true}, mountOptions{allAccounts: true}); err == nil || !strings.Contains(err.Error(), "account add") {
		t.Fatal("empty account store did not suggest account add", err)
	}
	// A star stays a literal saved-account name; it never changes selection.
	if _, _, _, err := authenticate(context.Background(), authFlags{account: "*", nonInteractive: true}); !errors.Is(err, accounts.ErrNotFound) {
		t.Fatal("account wildcard was given special meaning", err)
	}
}

func TestPrepareMountAccountsAndSessionLifecycleThroughHTTP(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var alphaRefreshes, bravoRefreshes, roots atomic.Int32
	var rejectBravo atomic.Bool
	var rejectBravoRoots atomic.Bool
	renewed := make(chan string, 16)
	const library = "gns://shared"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/anyshare/oauth2/login/refreshToken" {
			cookie, err := r.Cookie("client.oauth2_refresh_token")
			if err != nil {
				t.Error("refresh omitted account cookie")
				w.WriteHeader(401)
				return
			}
			name := strings.TrimPrefix(cookie.Value, "refresh-")
			var number int32
			switch name {
			case "alpha":
				number = alphaRefreshes.Add(1)
			case "bravo":
				number = bravoRefreshes.Add(1)
				if rejectBravo.Load() {
					w.WriteHeader(401)
					return
				}
			default:
				t.Error("unknown session identity", name)
			}
			http.SetCookie(w, &http.Cookie{Name: "client.oauth2_token", Value: fmt.Sprintf("access-%s-%d", name, number), Path: "/"})
			http.SetCookie(w, &http.Cookie{Name: "client.oauth2_refresh_token", Value: "refresh-" + name, Path: "/"})
			if number >= 2 {
				select {
				case renewed <- name:
				default:
				}
			}
			return
		}
		name := strings.SplitN(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer access-"), "-", 2)[0]
		if name != "alpha" && name != "bravo" {
			t.Error("API request used wrong session", name)
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/api/efast/v1/entry-doc-lib":
			roots.Add(1)
			if name == "bravo" && rejectBravoRoots.Load() {
				w.WriteHeader(403)
				return
			}
			fmt.Fprintf(w, `[{"id":%q,"name":%q,"size":-1,"type":"user_doc_lib"}]`, library, name+"-library")
		case "/api/efast/v1/folders/" + library + "/sub_objects":
			fmt.Fprintf(w, `{"dirs":[],"files":[{"docid":%q,"name":%q,"size":5,"rev":"same"}]}`, library+"/same", name+".txt")
		default:
			t.Error("unexpected mount preparation request", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	store, err := accounts.Default()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "bravo"} {
		if err := store.Put(accounts.Account{Name: name, Username: name, Password: "private-fixture", Server: server.URL, Session: auth.State{AccessToken: "old-" + name, RefreshToken: "refresh-" + name}}, name == "alpha", false); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	flags := authFlags{announce: true, nonInteractive: true, output: &output}
	opts := mountOptions{allAccounts: true, partSize: 8 << 20, timeout: time.Second}
	source, err := prepareMount(context.Background(), flags, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if source.root != nil || len(source.sessions) != 2 || roots.Load() != 2 || alphaRefreshes.Load() != 1 || bravoRefreshes.Load() != 1 {
		t.Fatal("not all saved accounts were checked before mounting")
	}
	for _, name := range []string{"alpha", "bravo"} {
		if !strings.Contains(output.String(), "Using account "+fmt.Sprintf("%q", name)) {
			t.Fatal("multi-account mount did not announce its accounts", output.String())
		}
	}
	if strings.Contains(output.String(), "private-fixture") || strings.Contains(output.String(), "access-") || strings.Contains(output.String(), "refresh-") {
		t.Fatal("account announcement exposed credentials")
	}
	entries, err := source.backend.Roots(context.Background())
	if err != nil || len(entries) != 2 {
		t.Fatal(entries, err)
	}
	for _, account := range entries {
		libraries, err := source.backend.List(context.Background(), account.ID)
		if err != nil || len(libraries) != 1 || libraries[0].Name != account.Name+"-library" {
			t.Fatal("account listing used wrong session", libraries, err)
		}
		files, err := source.backend.List(context.Background(), libraries[0].ID)
		if err != nil || len(files) != 1 || files[0].Name != account.Name+".txt" {
			t.Fatal("directory listing used wrong session", files, err)
		}
	}
	config, err := store.Load()
	if err != nil || config.Default != "alpha" {
		t.Fatal("aggregate selection changed the saved default", err)
	}
	// Renew both idle accounts. One expiration must identify that account and
	// permit the drivers to stop; cancellation must join all remaining workers.
	failed, stop := source.maintain(context.Background(), 10*time.Millisecond, &output)
	defer stop()
	seen := make(map[string]bool)
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for len(seen) != 2 {
		select {
		case name := <-renewed:
			seen[name] = true
		case <-timer.C:
			t.Fatal("idle renewal ignored an account")
		}
	}
	rejectBravo.Store(true)
	select {
	case err := <-failed:
		if !errors.Is(err, auth.ErrSessionExpired) || !strings.Contains(err.Error(), "bravo") {
			t.Error("expiration omitted account identity", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("expired account did not stop the aggregate mount")
	}
	stop()
	if alphaRefreshes.Load() < 2 || bravoRefreshes.Load() < 2 {
		t.Fatal("idle renewal ignored an account")
	}
	source.Close()
	for _, account := range source.sessions {
		if account.session.AccessToken() != "" {
			t.Fatal("mount retained an account session after close")
		}
	}
	rejectBravo.Store(false)
	rejectBravoRoots.Store(true)
	if failed, err := prepareMount(context.Background(), flags, opts); failed != nil || !errors.Is(err, anyshare.ErrPermission) || !strings.Contains(err.Error(), "bravo") {
		t.Fatal("startup silently dropped an unusable account", failed, err)
	}
}

func TestPrepareSingleAccountMountPreservesRootSelection(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const library = "gns://single"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/anyshare/oauth2/login/refreshToken":
			http.SetCookie(w, &http.Cookie{Name: "client.oauth2_token", Value: "access", Path: "/"})
			http.SetCookie(w, &http.Cookie{Name: "client.oauth2_refresh_token", Value: "refresh", Path: "/"})
		case "/api/efast/v1/entry-doc-lib":
			fmt.Fprintf(w, `[{"id":%q,"name":"Library","size":-1,"type":"user_doc_lib"}]`, library)
		default:
			t.Error("unexpected single-account request", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	store, err := accounts.Default()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(accounts.Account{Name: "single", Username: "single", Password: "private-fixture", Server: server.URL, Session: auth.State{RefreshToken: "refresh"}}, true, false); err != nil {
		t.Fatal(err)
	}
	source, err := prepareMount(context.Background(), authFlags{nonInteractive: true}, mountOptions{root: "/Library", partSize: 8 << 20, timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	client, ok := source.backend.(*anyshare.Client)
	if !ok || client.PartSize != 8<<20 || len(source.sessions) != 1 || source.root == nil || source.root.ID != library {
		t.Fatal("single-account mount changed root, upload policy or backend")
	}
}
