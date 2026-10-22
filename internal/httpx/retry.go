// Package httpx holds HTTP plumbing shared by the forge clients.
package httpx

import (
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

const (
	defaultMaxAttempts = 3
	defaultBaseDelay   = 500 * time.Millisecond
	defaultMaxDelay    = 10 * time.Second

	// DefaultTimeout bounds a single client call including retries, so a hung
	// forge connection cannot stall a poll cycle forever.
	DefaultTimeout = 60 * time.Second
)

// RetryTransport retries idempotent requests on transient failures: network
// errors, 429, 5xx, and GitHub's secondary rate-limit 403 (which carries
// Retry-After). POST is never retried since it may create duplicates.
type RetryTransport struct {
	next        http.RoundTripper
	maxAttempts int
	baseDelay   time.Duration
	maxDelay    time.Duration
}

func NewRetryTransport(next http.RoundTripper) *RetryTransport {
	if next == nil {
		next = http.DefaultTransport
	}
	return &RetryTransport{
		next:        next,
		maxAttempts: defaultMaxAttempts,
		baseDelay:   defaultBaseDelay,
		maxDelay:    defaultMaxDelay,
	}
}

func idempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions,
		http.MethodPut, http.MethodDelete, http.MethodPatch:
		return true
	}
	return false
}

func (t *RetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !idempotent(req.Method) || (req.Body != nil && req.Body != http.NoBody && req.GetBody == nil) {
		return t.next.RoundTrip(req)
	}

	for attempt := 1; ; attempt++ {
		resp, err := t.next.RoundTrip(req)
		if attempt == t.maxAttempts || req.Context().Err() != nil {
			return resp, err
		}
		delay, retry := t.backoff(attempt, resp, err)
		if !retry {
			return resp, err
		}
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}

		timer := time.NewTimer(delay)
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

// backoff reports whether the outcome is worth retrying and how long to wait.
// A Retry-After longer than maxDelay is not waited out: returning the error
// lets the caller's next poll cycle pick it up instead of blocking.
func (t *RetryTransport) backoff(attempt int, resp *http.Response, err error) (time.Duration, bool) {
	if err == nil {
		retryAfter, hasRetryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		switch {
		case resp.StatusCode == http.StatusTooManyRequests,
			resp.StatusCode >= 500,
			resp.StatusCode == http.StatusForbidden && hasRetryAfter:
		default:
			return 0, false
		}
		if hasRetryAfter {
			return retryAfter, retryAfter <= t.maxDelay
		}
	}
	d := t.baseDelay << (attempt - 1)
	d += rand.N(d/2 + 1)
	return min(d, t.maxDelay), true
}

func parseRetryAfter(v string) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	if at, err := http.ParseTime(v); err == nil {
		return max(time.Until(at), 0), true
	}
	return 0, false
}
