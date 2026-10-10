package anyshare

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

type testTokenSource struct {
	mu      sync.Mutex
	token   string
	refresh int
}

func (s *testTokenSource) AccessToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token
}

func (s *testTokenSource) Refresh(_ context.Context, rejected string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token == rejected {
		s.token = "new-access-token"
		s.refresh++
	}
	return nil
}

func TestRequestRefreshesAfterUnauthorized(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/api/efast/v1/entry-doc-lib" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") == "Bearer old-access-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	}))
	defer server.Close()
	source := &testTokenSource{token: "old-access-token"}
	client, err := New(server.URL, source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Roots(context.Background()); err != nil {
		t.Fatal(err)
	}
	source.mu.Lock()
	refreshes := source.refresh
	source.mu.Unlock()
	if requests != 2 || refreshes != 1 {
		t.Fatalf("requests=%d refreshes=%d, want one retry and one refresh", requests, refreshes)
	}
}
