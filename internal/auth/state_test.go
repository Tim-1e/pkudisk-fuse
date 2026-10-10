package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestRestoreRenewsAndPersistsSessionState(t *testing.T) {
	var mu sync.Mutex
	var refreshed State
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("client.oauth2_refresh_token")
		if err != nil || cookie.Value != "refresh-old" {
			t.Errorf("refresh cookie = %v, err = %v", cookie, err)
		}
		http.SetCookie(w, &http.Cookie{Name: "client.oauth2_token", Value: "access-new", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "client.oauth2_refresh_token", Value: "refresh-new", Path: "/"})
	}))
	defer server.Close()
	s, err := New(Options{Server: server.URL, IAAA: server.URL, OnState: func(state State) error {
		mu.Lock()
		defer mu.Unlock()
		refreshed = state
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Restore(context.Background(), State{AccessToken: "access-old", RefreshToken: "refresh-old"}); err != nil {
		t.Fatal(err)
	}
	if s.AccessToken() != "access-new" {
		t.Fatalf("access token = %q", s.AccessToken())
	}
	mu.Lock()
	defer mu.Unlock()
	if refreshed.AccessToken != "access-new" || refreshed.RefreshToken != "refresh-new" {
		t.Fatalf("persisted state = %#v", refreshed)
	}
}

func TestRestoreRejectsInvalidCookieValue(t *testing.T) {
	s, err := New(Options{Server: "http://127.0.0.1:1", IAAA: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = s.Restore(context.Background(), State{RefreshToken: "bad\nvalue"})
	if err == nil || !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("restore error = %v", err)
	}
}
