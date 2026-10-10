package anyshare

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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

func TestAPIUsesVerifiedHTTP2(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("protocol = %s, want HTTP/2", r.Proto)
		}
		io.WriteString(w, "[]")
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	client, err := New(server.URL, &testTokenSource{token: "test-access"})
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	tr := client.api.Transport.(*http.Transport)
	tr.TLSClientConfig = &tls.Config{RootCAs: roots}
	defer tr.CloseIdleConnections()
	if _, err := client.Roots(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRequestReturnsEOFWithoutNetworkRetry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	defer server.Close()
	client, err := New(server.URL, &testTokenSource{token: "test-access"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Roots(context.Background())
	if !errors.Is(err, io.EOF) || calls.Load() != 1 {
		t.Fatalf("error = %v, calls = %d", err, calls.Load())
	}
	if strings.Contains(err.Error(), "test-access") {
		t.Fatal("error exposed the access token")
	}
}
