package anyshare

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestStorageQuotaSumsWritableAndReadOnlyLibraries(t *testing.T) {
	quotas := map[string]Quota{"gns://custom": {Allocated: 12 << 40, Used: 2 << 40}, "gns://public": {Allocated: 200 << 30, Used: 3 << 30}, "gns://user": {Allocated: 200 << 30, Used: 70 << 30}}
	var publicAllocated atomic.Int64
	publicAllocated.Store(quotas["gns://public"].Allocated)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/efast/v1/entry-doc-lib":
			fmt.Fprint(w, `[{"id":"gns://custom","name":"12TB library","type":"custom_doc_lib"},{"id":"gns://public","name":"Public library","type":"department_doc_lib"},{"id":"gns://user","name":"Personal library","type":"user_doc_lib"}]`)
		case strings.HasPrefix(r.URL.Path, "/api/efast/v1/quota/doc-lib/"):
			if r.Method != "GET" {
				t.Error("library quota must use GET")
			}
			id := strings.TrimPrefix(r.URL.Path, "/api/efast/v1/quota/doc-lib/")
			quota := quotas[id]
			if id == "gns://public" {
				quota.Allocated = publicAllocated.Load()
			}
			json.NewEncoder(w).Encode(quota)
		case r.URL.Path == "/api/eacp/v1/perm1/checkall":
			var body struct {
				ID string `json:"docid"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			allow := []string{"download"}
			if body.ID != "gns://public" && body.ID != "gns://user/readonly" {
				allow = append(allow, "create", "modify")
			}
			// Deliberately conflicting allowvalue confirms that named permissions win.
			json.NewEncoder(w).Encode(map[string]any{"allow": allow, "allowvalue": []string{"create"}})
		default:
			t.Error("unexpected quota path", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c, _ := New(server.URL, &testTokenSource{token: "token"})
	quota, err := c.Quota(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := Quota{Allocated: (12 << 40) + (200 << 30) + (3 << 30), Used: (2 << 40) + (70 << 30) + (3 << 30)}
	if quota != want || quota.Available() != (10<<40)+(130<<30) {
		t.Fatal("wrong aggregate quota", quota)
	}
	publicAllocated.Store(1 << 60)
	if after, err := c.Quota(context.Background()); err != nil || after != quota {
		t.Fatal("read-only quota changed mounted capacity", after, err)
	}
	for _, test := range []struct {
		root string
		ro   bool
		want Quota
	}{
		{"gns://custom/photos", false, quotas["gns://custom"]},
		{"gns://public", false, Quota{Allocated: 3 << 30, Used: 3 << 30}},
		{"gns://user/readonly", false, Quota{Allocated: 70 << 30, Used: 70 << 30}},
		{"", true, Quota{Allocated: want.Used, Used: want.Used}},
	} {
		got, err := c.StorageQuota(context.Background(), test.root, test.ro)
		if err != nil || got != test.want {
			t.Fatalf("root=%q readOnly=%v got=%+v err=%v", test.root, test.ro, got, err)
		}
	}
}
