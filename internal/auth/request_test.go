package auth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAuthenticationUsesVerifiedHTTP2(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("protocol = %s, want HTTP/2", r.Proto)
		}
		io.WriteString(w, "login page")
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	s, err := New(Options{Server: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	s.http.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: roots}
	b, _, err := s.get(context.Background(), server.URL)
	if err != nil || string(b) != "login page" {
		t.Fatalf("body = %q, error = %v", b, err)
	}
}

func TestAuthenticationReturnsEOFWithoutReplayingRequests(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				io.Copy(io.Discard, r.Body)
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				conn.Close()
			}))
			defer server.Close()
			s, err := New(Options{Server: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			var body io.Reader
			if method == http.MethodPost {
				body = strings.NewReader("password=test-password")
			}
			_, _, err = s.request(context.Background(), method, server.URL+"/oauth2/signin?token=test-token", "application/x-www-form-urlencoded", body)
			if !errors.Is(err, io.EOF) || calls.Load() != 1 {
				t.Fatalf("error = %v, calls = %d", err, calls.Load())
			}
			if strings.Contains(err.Error(), "test-token") || strings.Contains(err.Error(), "test-password") {
				t.Fatal("error exposed credentials")
			}
			if !strings.Contains(err.Error(), method+" "+s.base.Host+"/oauth2/signin") {
				t.Fatal("error omitted the failing endpoint")
			}
		})
	}
}
