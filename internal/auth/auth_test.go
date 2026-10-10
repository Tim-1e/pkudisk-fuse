package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func checkPassword(t *testing.T, key *rsa.PrivateKey, encoded string) {
	t.Helper()
	cipher, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Error(err)
		return
	}
	plain, err := rsa.DecryptPKCS1v15(rand.Reader, key, cipher)
	if err != nil {
		t.Error(err)
		return
	}
	if !bytes.Equal(plain, []byte("test password")) {
		t.Error("password was not correctly RSA-encrypted")
	}
}

func TestConcurrentRefresh(t *testing.T) {
	var refreshes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		if _, err := r.Cookie("client.oauth2_refresh_token"); err != nil {
			t.Error("missing refresh token")
		}
		http.SetCookie(w, &http.Cookie{Name: "client.oauth2_token", Value: "new-test-token", Path: "/"})
	}))
	defer server.Close()
	session, err := New(Options{Server: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	session.token = "old-test-token"
	session.http.Jar.SetCookies(session.base, []*http.Cookie{{Name: "client.oauth2_refresh_token", Value: "refresh-test-token", Path: "/"}})
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := session.Refresh(context.Background(), "old-test-token"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if refreshes.Load() != 1 || session.AccessToken() != "new-test-token" {
		t.Fatal("concurrent refresh was not serialized")
	}
}

func TestIAAASSOLoginWithOTP(t *testing.T) {
	key, public := testKey(t)
	var server *httptest.Server
	var otpAsked atomic.Int32
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/anyshare/oauth2/login":
			http.Redirect(w, r, "/oauth2/signin?login_challenge=challenge", http.StatusFound)
		case "/oauth2/signin":
			if r.URL.Query().Get("token") == "iaaa-test-token" {
				if c, err := r.Cookie("login_challenge"); err != nil || c.Value != "challenge" {
					t.Error("SSO challenge missing")
				}
				http.SetCookie(w, &http.Cookie{Name: "client.oauth2_token", Value: "campus-test-token", Path: "/"})
				return
			}
			fmt.Fprintf(w, `<script id="__NEXT_DATA__">{"props":{"pageProps":{"challenge":"challenge","authconfig":{"thirdauth":{"config":{"authServer":"%s/web/v1/sso?ty=auth"}}}}}}</script>`, server.URL)
		case "/web/v1/sso":
			fmt.Fprintf(w, `<form action='%s/iaaa/oauth.jsp' method=post><input type=hidden name=appID value=anyshare><input name=redirectUrl value='%s/oauth2/signin'></form>`, server.URL, server.URL)
		case "/iaaa/oauth.jsp":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("appID") != "anyshare" {
				t.Error("wrong IAAA app")
			}
			fmt.Fprint(w, "IAAA login")
		case "/iaaa/getPublicKey.do":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "key": public})
		case "/iaaa/isShowCode.do":
			fmt.Fprint(w, `{"success":false}`)
		case "/iaaa/isMobileAuthen.do":
			if r.URL.Query().Get("userName") != "test-account" {
				t.Error("missing IAAA username")
			}
			fmt.Fprint(w, `{"success":true,"isMobileAuthen":true,"authenMode":"OTP"}`)
		case "/iaaa/oauthlogin.do":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("userName") != "test-account" || r.Form.Get("otpCode") != "123456" || r.Form.Get("appid") != "anyshare" {
				t.Error("incorrect IAAA login fields")
			}
			checkPassword(t, key, r.Form.Get("password"))
			fmt.Fprint(w, `{"success":true,"token":"iaaa-test-token"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	s, err := New(Options{Server: server.URL, IAAA: server.URL, Challenge: func(context.Context, Challenge) (string, error) { otpAsked.Add(1); return "123456", nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Login(context.Background(), Credentials{Username: "test-account", Password: []byte("test password")}); err != nil {
		t.Fatal(err)
	}
	if s.AccessToken() != "campus-test-token" || otpAsked.Load() != 1 {
		t.Fatal("missing campus session or OTP")
	}
}

func TestRedirectErrorsDoNotExposeTokens(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://unexpected.example/callback?token=redacted-test-token", http.StatusFound)
	}))
	defer server.Close()
	s, err := New(Options{Server: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.get(context.Background(), server.URL)
	if err == nil || strings.Contains(err.Error(), "redacted-test-token") {
		t.Fatal("unsafe authentication error", err)
	}
}

func TestCredentialsAreRequired(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "changed login page") }))
	defer server.Close()
	s, _ := New(Options{Server: server.URL})
	if err := s.Login(context.Background(), Credentials{}); err == nil {
		t.Fatal("login accepted missing credentials")
	}
}

func TestRefreshKeepsRotatedRefreshCookie(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		cookie, err := r.Cookie("client.oauth2_refresh_token")
		if err != nil || cookie.Value != fmt.Sprintf("refresh-test-%d", n-1) {
			t.Error("refresh rotation was not preserved")
		}
		http.SetCookie(w, &http.Cookie{Name: "client.oauth2_token", Value: fmt.Sprintf("access-test-%d", n), Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "client.oauth2_refresh_token", Value: fmt.Sprintf("refresh-test-%d", n), Path: "/"})
	}))
	defer server.Close()
	s, err := New(Options{Server: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	s.token = "access-test-0"
	s.http.Jar.SetCookies(s.base, []*http.Cookie{{Name: "client.oauth2_refresh_token", Value: "refresh-test-0", Path: "/"}})
	for i := 0; i < 2; i++ {
		if err := s.Refresh(context.Background(), s.AccessToken()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIdleSessionRenewalRetriesNetworkFailure(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "client.oauth2_token", Value: "renewed-test-token", Path: "/"})
	}))
	defer server.Close()
	s, err := New(Options{Server: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	s.token = "old-test-token"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	errorsSeen := make(chan error, 1)
	go func() {
		defer close(done)
		s.KeepAlive(ctx, 5*time.Millisecond, func(err error) {
			select {
			case errorsSeen <- err:
			default:
			}
		})
	}()
	defer func() { cancel(); <-done }()
	select {
	case err := <-errorsSeen:
		if errors.Is(err, ErrSessionExpired) {
			t.Fatal("transient failure expired the session")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idle session was not renewed")
	}
	deadline := time.Now().Add(2 * time.Second)
	for s.AccessToken() != "renewed-test-token" {
		if time.Now().After(deadline) {
			t.Fatal("renewal did not retry after transient failure")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRejectedSessionStopsRenewal(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusUnauthorized) }))
	defer server.Close()
	s, err := New(Options{Server: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	s.token = "old-test-token"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var reported error
	s.KeepAlive(ctx, 5*time.Millisecond, func(err error) { reported = err })
	if !errors.Is(reported, ErrSessionExpired) || calls.Load() != 1 {
		t.Fatal("rejected credentials were not reported once")
	}
}

func TestRefreshRejectsLoginPageWithoutNewToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "<html>login required</html>") }))
	defer server.Close()
	s, err := New(Options{Server: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	s.token = "old-test-token"
	s.http.Jar.SetCookies(s.base, []*http.Cookie{{Name: "client.oauth2_token", Value: "old-test-token", Path: "/"}})
	if err := s.Refresh(context.Background(), s.AccessToken()); !errors.Is(err, ErrSessionExpired) {
		t.Fatal("a login page was mistaken for a renewed session")
	}
}
