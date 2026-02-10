package gitea

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// writeJSON encodes v as JSON to w, failing the test on error.
// Safe to call from httptest handler goroutines (uses t.Error, not t.Fatal).
func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("failed to encode JSON response: %v", err)
	}
}

func TestListUserRepos(t *testing.T) {
	t.Run("single page", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/user/repos" {
				t.Errorf("unexpected path: %s", r.URL.Path)
			}
			repos := []Repo{
				{FullName: "org/app", Owner: RepoOwner{Login: "org"}, Name: "app", Permissions: RepoPermissions{Admin: true}},
				{FullName: "org/lib", Owner: RepoOwner{Login: "org"}, Name: "lib", Permissions: RepoPermissions{Admin: false}},
			}
			writeJSON(t, w, repos)
		}))
		defer srv.Close()

		client := NewHTTPClient(srv.URL, "test-token")
		repos, err := client.ListUserRepos(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(repos) != 2 {
			t.Fatalf("expected 2 repos, got %d", len(repos))
		}
		if repos[0].FullName != "org/app" {
			t.Errorf("expected org/app, got %s", repos[0].FullName)
		}
		if !repos[0].Permissions.Admin {
			t.Error("expected org/app to have admin permissions")
		}
		if repos[1].Permissions.Admin {
			t.Error("expected org/lib to NOT have admin permissions")
		}
	})

	t.Run("pagination", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			page := r.URL.Query().Get("page")
			pageNum, _ := strconv.Atoi(page)

			var repos []Repo
			if pageNum <= 1 {
				for i := range 50 {
					repos = append(repos, Repo{
						FullName: fmt.Sprintf("org/repo-%d", i),
						Owner:    RepoOwner{Login: "org"},
						Name:     fmt.Sprintf("repo-%d", i),
					})
				}
			} else {
				repos = append(repos, Repo{
					FullName: "org/repo-50",
					Owner:    RepoOwner{Login: "org"},
					Name:     "repo-50",
				})
			}

			writeJSON(t, w, repos)
		}))
		defer srv.Close()

		client := NewHTTPClient(srv.URL, "test-token")
		repos, err := client.ListUserRepos(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(repos) != 51 {
			t.Fatalf("expected 51 repos, got %d", len(repos))
		}
	})

	t.Run("empty result", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, []Repo{})
		}))
		defer srv.Close()

		client := NewHTTPClient(srv.URL, "test-token")
		repos, err := client.ListUserRepos(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(repos) != 0 {
			t.Fatalf("expected 0 repos, got %d", len(repos))
		}
	})

	t.Run("API error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "internal error", http.StatusInternalServerError)
		}))
		defer srv.Close()

		client := NewHTTPClient(srv.URL, "test-token")
		_, err := client.ListUserRepos(context.Background())
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("auth header sent", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			if auth != "token my-secret-token" {
				t.Errorf("expected 'token my-secret-token', got %q", auth)
			}
			writeJSON(t, w, []Repo{})
		}))
		defer srv.Close()

		client := NewHTTPClient(srv.URL, "my-secret-token")
		_, _ = client.ListUserRepos(context.Background())
	})
}

func TestGetRepoTopics(t *testing.T) {
	t.Run("repo with topics", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/repos/org/app/topics" {
				t.Errorf("unexpected path: %s", r.URL.Path)
			}
			writeJSON(t, w, map[string][]string{
				"topics": {"merge-queue", "nix", "go"},
			})
		}))
		defer srv.Close()

		client := NewHTTPClient(srv.URL, "test-token")
		topics, err := client.GetRepoTopics(context.Background(), "org", "app")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(topics) != 3 {
			t.Fatalf("expected 3 topics, got %d", len(topics))
		}
		if topics[0] != "merge-queue" {
			t.Errorf("expected first topic 'merge-queue', got %q", topics[0])
		}
	})

	t.Run("repo with no topics", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, map[string][]string{
				"topics": {},
			})
		}))
		defer srv.Close()

		client := NewHTTPClient(srv.URL, "test-token")
		topics, err := client.GetRepoTopics(context.Background(), "org", "app")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(topics) != 0 {
			t.Fatalf("expected 0 topics, got %d", len(topics))
		}
	})

	t.Run("API error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "not found", http.StatusNotFound)
		}))
		defer srv.Close()

		client := NewHTTPClient(srv.URL, "test-token")
		_, err := client.GetRepoTopics(context.Background(), "org", "nonexistent")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}
