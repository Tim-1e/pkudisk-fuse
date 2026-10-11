package diagnostic

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptrace"
	"regexp"
	"sync"
	"sync/atomic"
)

type transport struct {
	logger *Logger
	base   http.RoundTripper
	kind   string
}

var byteRange = regexp.MustCompile(`^bytes=[0-9]+-[0-9]+$`)

func (l *Logger) Transport(base http.RoundTripper, kind string) http.RoundTripper {
	if l == nil {
		return base
	}
	return &transport{logger: l, base: base, kind: kind}
}

func (t *transport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	l := t.logger
	endpoint := req.URL.Host + req.URL.EscapedPath()
	if t.kind == "object" {
		endpoint = req.URL.Host + " (signed path/query omitted)"
	}
	ctx, finish := l.Begin(req.Context(), "HTTP %s %s %q", t.kind, req.Method, endpoint)
	if value := req.Header.Get("Range"); value != "" {
		if byteRange.MatchString(value) {
			l.Printf(ctx, "requested range=%q", value)
		} else {
			l.Printf(ctx, "unrecognized range header omitted")
		}
	}
	trace := &httptrace.ClientTrace{
		DNSStart:          func(info httptrace.DNSStartInfo) { l.Printf(ctx, "resolving host %q", info.Host) },
		DNSDone:           func(info httptrace.DNSDoneInfo) { l.Printf(ctx, "DNS result: %s", Status(info.Err)) },
		ConnectStart:      func(network, addr string) { l.Printf(ctx, "connecting via %s to %q", network, addr) },
		ConnectDone:       func(network, addr string, err error) { l.Printf(ctx, "connection result: %s", Status(err)) },
		TLSHandshakeStart: func() { l.Printf(ctx, "TLS handshake started") },
		TLSHandshakeDone:  func(_ tls.ConnectionState, err error) { l.Printf(ctx, "TLS handshake: %s", Status(err)) },
		GotConn:           func(info httptrace.GotConnInfo) { l.Printf(ctx, "connection acquired; reused=%t", info.Reused) },
		WroteHeaders:      func() { l.Printf(ctx, "request headers sent; sending request body if present") },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			l.Printf(ctx, "request sent: %s; waiting for response headers", Status(info.Err))
		},
	}
	resp, err := t.base.RoundTrip(req.Clone(httptrace.WithClientTrace(ctx, trace)))
	if err != nil {
		finish("%s", Status(err))
		return resp, err
	}
	l.Printf(ctx, "response headers: HTTP %d %s; body=%s; receiving body", resp.StatusCode, resp.Proto, Bytes(resp.ContentLength))
	// Content-Range is deliberately not echoed: a server may return arbitrary
	// text. The read planner logs the trusted requested range and size instead.
	resp.Body = &responseBody{ReadCloser: resp.Body, logger: l, ctx: ctx, finish: finish, expected: resp.ContentLength}
	return resp, nil
}

type responseBody struct {
	io.ReadCloser
	logger   *Logger
	ctx      context.Context
	finish   func(string, ...any)
	expected int64
	bytes    atomic.Int64
	first    sync.Once
	done     sync.Once
}

func (b *responseBody) Read(data []byte) (int, error) {
	n, err := b.ReadCloser.Read(data)
	b.bytes.Add(int64(n))
	if n > 0 {
		b.first.Do(func() { b.logger.Printf(b.ctx, "response body started arriving") })
	}
	if err != nil {
		b.done.Do(func() {
			if err == io.EOF {
				b.finish("body received=%s; expected=%s; stream ended", Bytes(b.bytes.Load()), Bytes(b.expected))
			} else {
				b.finish("body received=%s; %s", Bytes(b.bytes.Load()), Status(err))
			}
		})
	}
	return n, err
}

func (b *responseBody) Close() error {
	err := b.ReadCloser.Close()
	b.done.Do(func() {
		b.finish("body closed; received=%s; expected=%s; %s", Bytes(b.bytes.Load()), Bytes(b.expected), Status(err))
	})
	return err
}
