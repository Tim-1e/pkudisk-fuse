package auth

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testTLSSession(t *testing.T, server *httptest.Server) *Session {
	t.Helper()
	t.Cleanup(server.Close)
	s, err := New(Options{Server: server.URL, IAAA: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	// Trust this server's certificate without disabling verification.
	s.http.Transport.(*retryTransport).TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	return s
}

func TestAuthenticationRetriesTLSHandshakeEOF(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			var handshakes, requests atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != method {
					t.Errorf("method = %s, want %s", r.Method, method)
				}
				if cookie, err := r.Cookie("test_session"); err != nil || cookie.Value != "test-session" {
					t.Error("retry lost the session cookie")
				}
				if method == http.MethodPost {
					if err := r.ParseForm(); err != nil || r.Form.Get("password") != "test password" {
						t.Error("retry lost the POST body")
					}
				}
				io.WriteString(w, "login page")
			}))
			server.Config.ErrorLog = log.New(io.Discard, "", 0)
			server.TLS = &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
				if handshakes.Add(1) <= 2 {
					hello.Conn.Close()
					return nil, io.EOF
				}
				return nil, nil
			}}
			server.StartTLS()
			s := testTLSSession(t, server)
			s.http.Jar.SetCookies(s.base, []*http.Cookie{{Name: "test_session", Value: "test-session", Path: "/"}})
			var body io.Reader
			if method == http.MethodPost {
				body = strings.NewReader(url.Values{"password": {"test password"}}.Encode())
			}
			b, _, err := s.request(context.Background(), method, server.URL+"/iaaa/oauth.jsp", "application/x-www-form-urlencoded", body)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != "login page" || handshakes.Load() != 3 || requests.Load() != 1 {
				t.Fatalf("body = %q, handshakes = %d, requests = %d", b, handshakes.Load(), requests.Load())
			}
		})
	}
}

func TestAuthenticationLimitsTLSHandshakeRetries(t *testing.T) {
	var handshakes atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("HTTP request sent after a failed TLS handshake")
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		handshakes.Add(1)
		hello.Conn.Close()
		return nil, io.EOF
	}}
	server.StartTLS()
	s := testTLSSession(t, server)
	_, _, err := s.form(context.Background(), server.URL+"/iaaa/oauthlogin.do?token=test-sso-token&userName=test-account", url.Values{"password": {"test password"}})
	if !errors.Is(err, io.EOF) || handshakes.Load() != 3 {
		t.Fatalf("error = %v, handshakes = %d", err, handshakes.Load())
	}
	if !strings.Contains(err.Error(), "POST "+s.base.Host+"/iaaa/oauthlogin.do") {
		t.Fatal("transport error omitted the failing endpoint", err)
	}
	for _, secret := range []string{"test-sso-token", "test-account", "test password"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("transport error exposed credentials")
		}
	}
}

func TestAuthenticationDoesNotRetrySubmittedRequest(t *testing.T) {
	for _, method := range []string{http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				io.Copy(io.Discard, r.Body)
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				// Simulate a server that processes a login/token request and then
				// closes the connection before returning an HTTP response.
				conn.Close()
			}))
			s := testTLSSession(t, server)
			var body io.Reader
			if method == http.MethodPost {
				body = strings.NewReader("password=test-password")
			}
			_, _, err := s.request(context.Background(), method, server.URL+"/iaaa/oauthlogin.do", "application/x-www-form-urlencoded", body)
			if !errors.Is(err, io.EOF) || requests.Load() != 1 {
				t.Fatalf("error = %v, requests = %d", err, requests.Load())
			}
		})
	}
}

func TestAuthenticationRetriesResponseEOFForGET(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		io.WriteString(w, "login page")
	}))
	s := testTLSSession(t, server)
	_, _, err := s.get(context.Background(), server.URL+"/anyshare/oauth2/login")
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d, want 2", requests.Load())
	}
}

func TestAuthenticationCancelsHandshakeRetries(t *testing.T) {
	var handshakes atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("cancelled login sent an HTTP request")
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		handshakes.Add(1)
		hello.Conn.Close()
		return nil, io.EOF
	}}
	server.StartTLS()
	s := testTLSSession(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
		if errors.Is(err, io.EOF) {
			// Cancel during the backoff, before another connection is made.
			time.AfterFunc(10*time.Millisecond, cancel)
		}
	}})
	_, _, err := s.get(ctx, server.URL)
	if !errors.Is(err, context.Canceled) || handshakes.Load() != 1 {
		t.Fatalf("error = %v, handshakes = %d", err, handshakes.Load())
	}
}
