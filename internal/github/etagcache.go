package github

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync"
)

// etagCache revalidates GET responses with If-None-Match. GitHub does not
// charge a 304 against the primary rate limit, so unchanged reads are free. On
// a 304 it replays the cached body, so upstream always sees a 200.
type etagCache struct {
	next    http.RoundTripper
	maxSize int

	mu      sync.Mutex
	entries map[string]*etagEntry
}

type etagEntry struct {
	etag   string
	body   []byte
	header http.Header
	status int
}

func newETagCache(next http.RoundTripper, maxSize int) *etagCache {
	if next == nil {
		next = http.DefaultTransport
	}
	return &etagCache{next: next, maxSize: maxSize, entries: map[string]*etagEntry{}}
}

// cacheKey includes Accept because it selects the media type: different
// representations of the same URL must not share an entry.
func cacheKey(req *http.Request) string {
	return req.Method + " " + req.URL.String() + " " + req.Header.Get("Accept")
}

func (c *etagCache) get(key string) *etagEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[key]
}

func (c *etagCache) put(key string, e *etagEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Drop everything when full: entries refill for free (a 304), so a coarse
	// reset beats tracking LRU state.
	if len(c.entries) >= c.maxSize {
		c.entries = map[string]*etagEntry{}
	}
	c.entries[key] = e
}

func (c *etagCache) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		return c.next.RoundTrip(req)
	}

	key := cacheKey(req)
	cached := c.get(key)
	if cached != nil {
		req = req.Clone(req.Context())
		req.Header.Set("If-None-Match", cached.etag)
	}

	resp, err := c.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusNotModified && cached != nil {
		_ = resp.Body.Close()
		return cached.response(req, resp.Header), nil
	}

	etag := resp.Header.Get("ETag")
	if resp.StatusCode == http.StatusOK && etag != "" {
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		c.put(key, &etagEntry{
			etag:   etag,
			body:   body,
			header: resp.Header.Clone(),
			status: resp.StatusCode,
		})
		resp.Body = io.NopCloser(bytes.NewReader(body))
	}
	return resp, nil
}

// response rebuilds a *http.Response from a cached entry, overlaying the 304's
// live rate-limit headers so downstream accounting stays accurate.
func (e *etagEntry) response(req *http.Request, liveHeader http.Header) *http.Response {
	h := e.header.Clone()
	for k, v := range liveHeader {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), "X-Ratelimit") {
			h[http.CanonicalHeaderKey(k)] = v
		}
	}
	return &http.Response{
		Status:     http.StatusText(e.status),
		StatusCode: e.status,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     h,
		Body:       io.NopCloser(bytes.NewReader(e.body)),
		Request:    req,
	}
}
