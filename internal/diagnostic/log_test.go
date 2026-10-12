package diagnostic

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

type capturedLog struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *capturedLog) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(data)
}
func (b *capturedLog) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

func TestDisabledDiagnosticsPreserveContextAndTransport(t *testing.T) {
	var logger *Logger
	ctx := context.Background()
	got, finish := logger.Begin(ctx, "disabled")
	finish("disabled")
	logger.Printf(ctx, "disabled")
	if got != ctx || logger.Transport(http.DefaultTransport, "API") != http.DefaultTransport {
		t.Fatal("disabled diagnostics changed request handling")
	}
}

func TestNestedOperationsAreCorrelated(t *testing.T) {
	var output capturedLog
	logger := New(&output)
	ctx, finish := logger.Begin(context.Background(), "read photo")
	child, finishChild := logger.Begin(ctx, "fetch range")
	logger.Printf(child, "waiting for server")
	finishChild("success")
	finish("returned bytes")
	for _, want := range []string{"[#1] START read photo (parent=#0)", "[#2] START fetch range (parent=#1)", "[#2] waiting for server", "[#2] END fetch range: success; elapsed=", "[#1] END read photo: returned bytes; elapsed="} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing diagnostic %q", want)
		}
	}
}

func TestHTTPLogsHeadersBeforeBodyWaitAndRedactsSecrets(t *testing.T) {
	for _, kind := range []string{"auth", "API", "object"} {
		t.Run(kind, func(t *testing.T) {
			var output capturedLog
			logger := New(&output)
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", "6")
				w.Header().Set("Set-Cookie", "session=response-cookie-secret")
				w.Header().Set("Content-Range", "response-range-secret")
				w.WriteHeader(http.StatusPartialContent)
				w.(http.Flusher).Flush()
				<-release
				io.WriteString(w, "abcdef")
			}))
			defer server.Close()
			defer unblock()
			path := "/endpoint"
			if kind == "object" {
				path = "/signed-path-secret"
			}
			req, err := http.NewRequest("POST", server.URL+path+"?signature=query-secret", strings.NewReader("password=body-secret"))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer token-secret")
			req.Header.Set("Cookie", "session=cookie-secret")
			req.Header.Set("Range", "bytes=0-5")
			client := &http.Client{Transport: logger.Transport(http.DefaultTransport, kind)}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			log := output.String()
			for _, want := range []string{"START HTTP " + kind, "requested range=\"bytes=0-5\"", "waiting for response headers", "response headers: HTTP 206", "receiving body"} {
				if !strings.Contains(log, want) {
					t.Fatalf("missing diagnostic before body arrives: %q", want)
				}
			}
			if strings.Contains(log, "END HTTP") {
				t.Fatal("HTTP request was declared complete while its body was blocked")
			}
			unblock()
			data, err := io.ReadAll(resp.Body)
			if err != nil || string(data) != "abcdef" || resp.StatusCode != 206 {
				t.Fatal("diagnostics changed response", err)
			}
			if !strings.Contains(output.String(), "response body started arriving") || strings.Count(output.String(), "END HTTP") != 1 {
				t.Fatal("body lifetime was not logged exactly once")
			}
			for _, secret := range []string{"signed-path-secret", "query-secret", "body-secret", "token-secret", "cookie-secret", "response-cookie-secret", "response-range-secret"} {
				if strings.Contains(output.String(), secret) {
					t.Fatalf("diagnostics exposed %s", secret)
				}
			}
		})
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestHTTPTransportErrorPreservedWithoutSensitiveText(t *testing.T) {
	var output capturedLog
	logger := New(&output)
	sensitive := &url.Error{Op: "Get", URL: "https://objects.example/private-path?token=transport-secret", Err: io.ErrUnexpectedEOF}
	req, _ := http.NewRequest("GET", "https://objects.example/private-path?token=query-secret", nil)
	wrapped := logger.Transport(transportFunc(func(*http.Request) (*http.Response, error) { return nil, sensitive }), "object")
	_, err := wrapped.RoundTrip(req)
	if err != sensitive || !strings.Contains(output.String(), "failed: unexpected EOF") {
		t.Fatal("transport diagnostics changed or hid the error class")
	}
	for _, secret := range []string{"transport-secret", "query-secret", "private-path"} {
		if strings.Contains(output.String(), secret) {
			t.Fatal("transport diagnostics exposed a URL")
		}
	}
	if text := Status(errors.New("response with password=arbitrary-secret")); strings.Contains(text, "arbitrary-secret") {
		t.Fatal("unknown error text was exposed")
	}
}

func TestDiagnosticSizesFollowBytesMode(t *testing.T) {
	var output bytes.Buffer
	logger := New(&output)
	if got := logger.Size(1536).String(); got != "1.5 KiB" {
		t.Fatal(got)
	}
	logger.SetBytes(true)
	if got := logger.Size(1536).String(); got != "1536 bytes" {
		t.Fatal(got)
	}
	if got := logger.Size(-1).String(); got != "unknown" {
		t.Fatal(got)
	}
	var disabled *Logger
	disabled.SetBytes(true)
	if got := disabled.Size(1536).String(); got != "1.5 KiB" {
		t.Fatal(got)
	}
}
