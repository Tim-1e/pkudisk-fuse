package auth

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// retryTransport retries an EOF while establishing a connection. GotConn runs
// after the TLS handshake, so these retries cannot replay an HTTP request that may
// already have submitted a password or consumed a one-time SSO token.
type retryTransport struct {
	*http.Transport
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		var connected atomic.Bool
		trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { connected.Store(true) }}
		traced := req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
		resp, err := t.Transport.RoundTrip(traced)
		if err != nil && req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		safeReplay := req.Method == http.MethodGet || req.Method == http.MethodHead
		if err == nil || attempt >= 2 || !retryableAuthTransportError(err) ||
			(connected.Load() && !safeReplay) || (req.Body != nil && req.GetBody == nil) {
			return resp, err
		}

		timer := time.NewTimer(time.Duration(1<<attempt) * 100 * time.Millisecond)
		select {
		case <-req.Context().Done():
			timer.Stop()
			return nil, req.Context().Err()
		case <-timer.C:
		}
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			req = req.Clone(req.Context())
			req.Body = body
		}
	}
}

func retryableAuthTransportError(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "timeout awaiting response headers") || strings.Contains(message, "connection reset by peer")
}

func retryableAuthRequest(method string, endpoint *url.URL, err error) bool {
	if method != http.MethodGet && method != http.MethodHead {
		return false
	}
	// The callback carries a one-time OAuth token; replaying it after the
	// server has processed the request could consume the token twice.
	if endpoint != nil && strings.HasPrefix(endpoint.Path, "/oauth2/signin") {
		return false
	}
	return retryableAuthTransportError(err)
}

func waitAuthRetry(ctx context.Context, retry int) error {
	delay := time.Duration(150*(1<<retry)) * time.Millisecond
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
