package httpx

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryTransport(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		fails      int32 // requests answered with failStatus before a 200
		failStatus int
		header     http.Header
		wantStatus int
		wantCalls  int32
	}{
		{"idempotent 5xx retried", http.MethodPatch, 2, http.StatusBadGateway, nil, http.StatusOK, 3},
		{"gives up after max attempts", http.MethodGet, 100, http.StatusServiceUnavailable, nil, http.StatusServiceUnavailable, defaultMaxAttempts},
		{"POST not retried", http.MethodPost, 1, http.StatusBadGateway, nil, http.StatusBadGateway, 1},
		{"4xx not retried", http.MethodGet, 1, http.StatusNotFound, nil, http.StatusNotFound, 1},
		{"rate-limit 403 with Retry-After retried", http.MethodGet, 1, http.StatusForbidden, http.Header{"Retry-After": {"0"}}, http.StatusOK, 2},
		{"Retry-After beyond cap not waited", http.MethodGet, 1, http.StatusTooManyRequests, http.Header{"Retry-After": {"3600"}}, http.StatusTooManyRequests, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if calls.Add(1) <= tt.fails {
					for k, v := range tt.header {
						w.Header()[k] = v
					}
					w.WriteHeader(tt.failStatus)
					return
				}
				_, _ = w.Write(body)
			}))
			defer srv.Close()

			tr := NewRetryTransport(nil)
			tr.baseDelay = time.Millisecond
			req, err := http.NewRequest(tt.method, srv.URL, strings.NewReader("payload"))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := (&http.Client{Transport: tr}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(resp.Body)

			if resp.StatusCode != tt.wantStatus || calls.Load() != tt.wantCalls {
				t.Fatalf("status = %d calls = %d, want %d / %d", resp.StatusCode, calls.Load(), tt.wantStatus, tt.wantCalls)
			}
			// The body must be replayed on every attempt.
			if resp.StatusCode == http.StatusOK && string(body) != "payload" {
				t.Fatalf("body = %q", body)
			}
		})
	}
}
