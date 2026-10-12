package anyshare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

const sharingPolicyFixture = `{"validity_period":{"default":30,"maximum":30},"perm":{"allow":["display","preview","download","create","modify","cache"],"default":["display","preview","download"]},"access_times":{"default":-1,"maximum":-1},"must_set_password":false,"verify_mobile":{"enable":true,"force":false}}`

func shareTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := New(server.URL, &testTokenSource{token: "fixture-token"})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestExternalLinkPolicyAndWireSettings(t *testing.T) {
	var body struct {
		Item struct {
			ID, Type string
			Allow    []string
		}
		Title, Password string
		ExpiresAt       time.Time `json:"expires_at"`
		LimitedTimes    int64     `json:"limited_times"`
		VerifyMobile    bool      `json:"verify_mobile"`
	}
	creates := 0
	client := shareTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/doc-share/v1/sharing-configuration-scope/anyone":
			if r.Method != "GET" {
				t.Error("policy is not a GET")
			}
			fmt.Fprint(w, sharingPolicyFixture)
		case "/api/shared-link/v1/document/anonymous":
			creates++
			if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&body) != nil {
				t.Error("invalid create request")
			}
			fmt.Fprint(w, `{"id":"link-1"}`)
		default:
			t.Error("unexpected endpoint", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	entry := Entry{ID: "gns://library/folder", Name: "中文 Photos", Dir: true}
	ttl, visits := time.Hour, int64(3)
	link, err := client.CreateExternalLink(context.Background(), entry, ShareOptions{Permissions: []string{"create", "modify"}, ExpiresIn: &ttl, Code: "Code1234", Limit: &visits, VerifyMobile: true})
	if err != nil || link.ID != "link-1" || link.Code != "Code1234" || link.LimitedTimes != 3 || !link.VerifyMobile {
		t.Fatal(link, err)
	}
	portal, err := url.Parse(link.URL)
	if err != nil || portal.Path != "/anyshare/zh-cn/link/link-1" || portal.Query().Get("item_id") != entry.ID || portal.Query().Get("item_type") != "folder" || portal.Query().Get("type") != "anonymous" || portal.Query().Get("password") != "" || portal.Query().Get("token") != "" {
		t.Fatal("share URL did not address the portal or exposed credentials", err)
	}
	if body.Item.ID != entry.ID || body.Item.Type != "folder" || !reflect.DeepEqual(body.Item.Allow, []string{"display", "create", "modify"}) || body.Password != link.Code || body.Title != entry.Name || body.LimitedTimes != 3 || !body.VerifyMobile || time.Until(body.ExpiresAt) < 59*time.Minute {
		t.Fatalf("wrong wire settings: %+v", body)
	}
	if _, err := client.CreateExternalLink(context.Background(), entry, ShareOptions{}); err != nil || body.LimitedTimes != -1 || time.Until(body.ExpiresAt) < 29*24*time.Hour || !reflect.DeepEqual(body.Item.Allow, []string{"display", "preview", "download"}) {
		t.Fatal("server defaults were lost", body, err)
	}
	for _, options := range []ShareOptions{{ExpiresIn: new(time.Duration)}, {Permissions: []string{"delete"}}, {ExpiresIn: durationPointer(31 * 24 * time.Hour)}} {
		before := creates
		if _, err := client.CreateExternalLink(context.Background(), entry, options); err == nil || creates != before {
			t.Fatal("forbidden settings reached mutation", options, err)
		}
	}
	entry.Dir = false
	if _, err := client.CreateExternalLink(context.Background(), entry, ShareOptions{Permissions: []string{"create"}}); err == nil || creates != 2 {
		t.Fatal("file received folder-upload permissions", err)
	}
}

func durationPointer(value time.Duration) *time.Duration { return &value }

func TestShareLinksPaginationExpiryAndRevocation(t *testing.T) {
	pages, deleted := 0, false
	client := shareTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			if r.URL.Path != "/api/shared-link/v1/document/anonymous/link-20" {
				t.Error("wrong link deletion", r.URL.Path)
			}
			deleted = true
			w.WriteHeader(204)
			return
		}
		if r.URL.Path != "/api/shared-link/v1/document/file/gns://library/file" || r.URL.Query().Get("type") != "anonymous" || r.URL.Query().Get("limit") != "20" {
			t.Error("wrong link listing", r.URL.String())
		}
		pages++
		if r.URL.Query().Get("offset") == "0" {
			items := make([]map[string]any, 20)
			for i := range items {
				items[i] = map[string]any{"id": fmt.Sprintf("link-%d", i), "expires_at": "1970-01-01T08:00:00+08:00", "limited_times": -1, "item": map[string]any{"allow": []string{"display", "download"}}}
			}
			json.NewEncoder(w).Encode(items)
		} else if r.URL.Query().Get("offset") == "20" {
			fmt.Fprint(w, `[{"id":"link-20","title":"test","password":"Code1234","expires_at":"2030-01-01T00:00:00Z","limited_times":5,"verify_mobile":true,"item":{"allow":["download"]}}]`)
		} else {
			t.Error("wrong pagination offset")
		}
	})
	links, err := client.ShareLinks(context.Background(), Entry{ID: "gns://library/file", Name: "file"}, true)
	if err != nil || len(links) != 21 || pages != 2 || links[0].ExpiresAt != nil || links[0].Title != "file" || links[20].ExpiresAt == nil || links[20].Code != "Code1234" || links[20].LimitedTimes != 5 || !links[20].VerifyMobile {
		t.Fatal(links, err)
	}
	if err := client.RemoveExternalLink(context.Background(), "link-20"); err != nil || !deleted {
		t.Fatal("link not revoked", err)
	}
	for _, id := range []string{"", "..", "bad/id", "https://example/link-20", "bad\nname"} {
		if err := client.RemoveExternalLink(context.Background(), id); err == nil {
			t.Fatal("unsafe link ID accepted", id)
		}
	}
}

func TestInternalLinkReuseDoesNotGrantPermissions(t *testing.T) {
	exists, creates := false, 0
	client := shareTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Query().Get("type") == "realname" {
			if exists {
				fmt.Fprint(w, `[{"id":"inner-1","type":"realname"}]`)
			} else {
				fmt.Fprint(w, `[]`)
			}
			return
		}
		if r.Method != "POST" || r.URL.Path != "/api/shared-link/v1/document/realname" {
			t.Error("navigation link changed permissions", r.Method, r.URL.Path)
			w.WriteHeader(404)
			return
		}
		var body map[string]map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["item"]["id"] != "gns://library/file" || body["item"]["type"] != "file" {
			t.Error("wrong navigation-link item")
		}
		exists = true
		creates++
		fmt.Fprint(w, `{"id":"inner-1"}`)
	})
	for range 2 {
		link, err := client.InternalLink(context.Background(), Entry{ID: "gns://library/file", Name: "file"})
		if err != nil || link.ID != "inner-1" {
			t.Fatal(link, err)
		}
	}
	if creates != 1 {
		t.Fatal("existing navigation link was duplicated")
	}
}

func TestGrantAndSelectiveRevokePreserveACL(t *testing.T) {
	const inherited = `{"accessorid":"recipient","accessortype":"user","accessorname":"Inherited","allow":["display"],"deny":["modify"],"endtime":-1,"inheritdocid":"gns://library","inheritpath":"/Library","read_policy":{"download":{"enable":false}}}`
	const unrelated = `{"accessorid":"keep","accessortype":"user","accessorname":"保留用户","allow":["download"],"deny":["delete"],"endtime":1893456000000000,"inheritdocid":"","read_policy":{"download":{"enable":true}},"unknown_field":{"must":"keep"}}`
	sets, grants := 0, 0
	client := shareTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/doc-share/v1/sharing-configuration-scope/user":
			fmt.Fprint(w, strings.ReplaceAll(sharingPolicyFixture, `"default":30,"maximum":30`, `"default":-1,"maximum":-1`))
		case "/api/doc-share/v1/doc-perm":
			grants++
			var body struct {
				DocID   string `json:"doc_id"`
				Configs []struct {
					Allow, Deny []string
					Accessor    Recipient
					ExpiresAt   time.Time `json:"expires_at"`
				}
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.DocID != "gns://library/file" || len(body.Configs) != 1 || body.Configs[0].Accessor.ID != "recipient" || body.Configs[0].Accessor.Type != "user" || body.Configs[0].ExpiresAt.Unix() != 0 || len(body.Configs[0].Deny) != 0 {
				t.Error("wrong grant request", body)
			}
		case "/api/eacp/v1/perm2/get":
			fmt.Fprintf(w, `{"inherit":true,"perminfos":[%s,%s,{"accessorid":"recipient","accessortype":"user","allow":["download"],"deny":[],"endtime":-1,"inheritdocid":""}]}`, inherited, unrelated)
		case "/api/eacp/v1/perm2/set":
			sets++
			var body struct {
				DocID     string
				Inherit   bool
				Perminfos []json.RawMessage
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.DocID != "gns://library/file" || !body.Inherit || len(body.Perminfos) != 2 {
				t.Error("revoke destroyed inheritance or unrelated rule", body)
			}
			for i, want := range []string{inherited, unrelated} {
				var actual, expected any
				json.Unmarshal(body.Perminfos[i], &actual)
				json.Unmarshal([]byte(want), &expected)
				if !reflect.DeepEqual(actual, expected) {
					t.Error("revoke changed retained raw policy/expiry fields")
				}
			}
			fmt.Fprint(w, `{"result":0}`)
		default:
			t.Error("unexpected endpoint or notification", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	recipients := []Recipient{{ID: "recipient", Type: "user"}}
	if err := client.GrantShare(context.Background(), Entry{ID: "gns://library/file"}, recipients, ShareOptions{}); err != nil || grants != 1 {
		t.Fatal(err)
	}
	acl, err := client.SharePermissions(context.Background(), "gns://library/file")
	if err != nil || acl.Permissions[1].ExpiresAt == nil || !acl.Permissions[1].ExpiresAt.Equal(time.UnixMicro(1893456000000000)) {
		t.Fatal("ACL timestamps were decoded incorrectly", acl, err)
	}
	if n, err := client.RevokeShare(context.Background(), "gns://library/file", recipients); err != nil || n != 1 || sets != 1 {
		t.Fatal(n, err)
	}
	if _, err := client.RevokeShare(context.Background(), "gns://library/file", []Recipient{{ID: "missing", Type: "user"}}); !errors.Is(err, ErrNotFound) || sets != 1 {
		t.Fatal("missing recipient caused partial revocation", err)
	}
}

func TestSharingNeverReportsApprovalOrMalformedResultsAsEffective(t *testing.T) {
	for _, response := range []string{`{"result":1}`, `{}`, `{"result":2}`} {
		client := shareTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/get") {
				fmt.Fprint(w, `{"inherit":true,"perminfos":[{"accessorid":"u","accessortype":"user","allow":["download"]}]}`)
			} else {
				fmt.Fprint(w, response)
			}
		})
		if _, err := client.RevokeShare(context.Background(), "gns://lib/file", []Recipient{{ID: "u", Type: "user"}}); err == nil || response == `{"result":1}` && !errors.Is(err, ErrPending) {
			t.Fatal("non-effective result reported success", response, err)
		}
	}
	client := shareTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "configuration") {
			fmt.Fprint(w, sharingPolicyFixture)
		} else {
			w.WriteHeader(202)
			fmt.Fprint(w, `{}`)
		}
	})
	if _, err := client.CreateExternalLink(context.Background(), Entry{ID: "gns://lib/file"}, ShareOptions{}); !errors.Is(err, ErrPending) {
		t.Fatal("approval reported as link creation", err)
	}
}

func TestSharingRequiredCodeAndMatchedDirectorySearch(t *testing.T) {
	client := shareTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "configuration") {
			fmt.Fprint(w, strings.ReplaceAll(sharingPolicyFixture, `"must_set_password":false`, `"must_set_password":true`))
			return
		}
		if r.URL.Path != "/api/eacp/v1/department/search" {
			t.Error("unexpected mutation before required code", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["key"] != "张同学" || body["start"] != float64(0) || body["limit"] != float64(50) {
			t.Error("wrong directory query")
		}
		fmt.Fprint(w, `{"userinfos":[{"userid":"u","name":"张同学","account":"directory-account","deppath":"学院"}],"depinfos":[{"depid":"d","name":"学院","path":"/学院"}]}`)
	})
	if _, err := client.CreateExternalLink(context.Background(), Entry{ID: "gns://lib/file"}, ShareOptions{}); !errors.Is(err, ErrPickupCodeRequired) {
		t.Fatal("required code omitted", err)
	}
	found, err := client.SearchRecipients(context.Background(), "张同学", 50)
	if err != nil || len(found) != 2 || found[0].Account != "directory-account" || found[0].ID != "u" || found[1].Type != "department" {
		t.Fatal("matched recipient data was lost", found, err)
	}
}
