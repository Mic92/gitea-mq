package github

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// countingTransport records how many requests reached the origin and echoes a
// fixed ETag so the cache can revalidate.
type countingTransport struct {
	origin int
	body   string
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.origin++
	rec := httptest.NewRecorder()
	if req.Header.Get("If-None-Match") == `"v1"` {
		rec.WriteHeader(http.StatusNotModified)
		rec.Header().Set("X-RateLimit-Remaining", "5000")
		resp := rec.Result()
		resp.Request = req
		return resp, nil
	}
	rec.Header().Set("ETag", `"v1"`)
	rec.WriteHeader(http.StatusOK)
	_, _ = rec.WriteString(t.body)
	resp := rec.Result()
	resp.Request = req
	return resp, nil
}

func TestETagCache_RevalidatesAndServesCachedBody(t *testing.T) {
	origin := &countingTransport{body: "hello"}
	c := newETagCache(origin, 16)

	do := func() (int, string) {
		req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/x", nil)
		resp, err := c.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode, string(b)
	}

	// First call populates the cache from a 200.
	if code, body := do(); code != 200 || body != "hello" {
		t.Fatalf("first call = %d %q, want 200 hello", code, body)
	}
	// Second call revalidates; origin returns 304 but caller still sees the body.
	if code, body := do(); code != 200 || body != "hello" {
		t.Fatalf("second call = %d %q, want 200 hello (from cache)", code, body)
	}
	if origin.origin != 2 {
		t.Fatalf("origin hit %d times, want 2 (both revalidations reach GitHub)", origin.origin)
	}
}

func TestETagCache_SendsIfNoneMatch(t *testing.T) {
	var lastINM string
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		lastINM = req.Header.Get("If-None-Match")
		rec := httptest.NewRecorder()
		rec.Header().Set("ETag", `"v1"`)
		rec.WriteHeader(http.StatusOK)
		resp := rec.Result()
		resp.Request = req
		return resp, nil
	})
	c := newETagCache(rt, 16)

	req1, _ := http.NewRequest(http.MethodGet, "https://api.github.com/y", nil)
	resp1, _ := c.RoundTrip(req1)
	_ = resp1.Body.Close()
	if lastINM != "" {
		t.Fatalf("first request sent If-None-Match=%q, want empty", lastINM)
	}

	req2, _ := http.NewRequest(http.MethodGet, "https://api.github.com/y", nil)
	resp2, _ := c.RoundTrip(req2)
	_ = resp2.Body.Close()
	if lastINM != `"v1"` {
		t.Fatalf("second request If-None-Match=%q, want \"v1\"", lastINM)
	}
}

// POST must bypass the cache entirely.
func TestETagCache_SkipsNonGET(t *testing.T) {
	var inm string
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		inm = req.Header.Get("If-None-Match")
		rec := httptest.NewRecorder()
		rec.Header().Set("ETag", `"v1"`)
		rec.WriteHeader(http.StatusOK)
		resp := rec.Result()
		resp.Request = req
		return resp, nil
	})
	c := newETagCache(rt, 16)

	for range 2 {
		req, _ := http.NewRequest(http.MethodPost, "https://api.github.com/z", nil)
		resp, _ := c.RoundTrip(req)
		_ = resp.Body.Close()
	}
	if inm != "" {
		t.Fatalf("POST sent If-None-Match=%q, want empty (not cached)", inm)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
