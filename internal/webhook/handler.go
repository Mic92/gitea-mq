// Package webhook implements the HTTP handler that receives Gitea webhook
// events (commit_status) and routes them to the check monitor.
package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/jogman/gitea-mq/internal/monitor"
	"github.com/jogman/gitea-mq/internal/queue"
	"github.com/jogman/gitea-mq/internal/store/pg"
)

// RepoMonitor holds the monitor deps for a single repo. The webhook handler
// routes events to the correct repo's monitor.
type RepoMonitor struct {
	Deps   *monitor.Deps
	RepoID int64
}

// RepoLookup abstracts how the webhook handler finds a repo's monitor.
// Implementations include the RepoRegistry (dynamic) and simple maps (tests).
type RepoLookup interface {
	LookupMonitor(fullName string) (*RepoMonitor, bool)
}

// MapRepoLookup adapts a static map to the RepoLookup interface.
type MapRepoLookup map[string]*RepoMonitor

// LookupMonitor returns the RepoMonitor for a given "owner/name" key.
func (m MapRepoLookup) LookupMonitor(fullName string) (*RepoMonitor, bool) {
	rm, ok := m[fullName]
	return rm, ok
}

// Handler returns an http.Handler that processes Gitea webhook events.
func Handler(secret string, repos RepoLookup, queueSvc *queue.Service) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}

		sig := r.Header.Get("X-Gitea-Signature")
		if !ValidateSignature(body, sig, secret) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var event statusEvent
		if err := json.Unmarshal(body, &event); err != nil {
			slog.Warn("malformed webhook payload", "error", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		if err := event.validate(); err != nil {
			slog.Warn("invalid webhook payload", "error", err)
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}

		// Ignore our own status updates to prevent feedback loops.
		if event.Context == "gitea-mq" {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Route to the correct repo.
		repoKey := event.Repository.FullName
		rm, ok := repos.LookupMonitor(repoKey)
		if !ok {
			slog.Debug("webhook for unmanaged repo", "repo", repoKey)
			w.WriteHeader(http.StatusOK)
			return
		}

		// Find the queue entry whose merge branch SHA matches this commit.
		// Only head-of-queue entries in "testing" state have merge branches.
		entry := findEntryForCommit(r.Context(), queueSvc, rm.RepoID, event.SHA)
		if entry == nil {
			// Status for a commit we're not tracking — ignore.
			w.WriteHeader(http.StatusOK)
			return
		}

		checkState := mapState(event.State)

		if err := monitor.ProcessCheckStatus(r.Context(), rm.Deps, entry, event.Context, checkState, event.TargetURL); err != nil {
			slog.Error("failed to process check status", "pr", entry.PrNumber, "error", err)
			// Still return 200 — Gitea will retry on non-2xx, which could
			// cause duplicate processing.
		}

		w.WriteHeader(http.StatusOK)
	})
}

// statusEvent is the subset of Gitea's commit_status webhook payload we need.
type statusEvent struct {
	SHA        string `json:"sha"`
	Context    string `json:"context"`
	State      string `json:"state"` // "pending", "success", "failure", "error"
	TargetURL  string `json:"target_url"`
	Repository struct {
		FullName string `json:"full_name"` // "owner/repo"
	} `json:"repository"`
}

func (e *statusEvent) validate() error {
	if e.SHA == "" {
		return fmt.Errorf("missing sha")
	}
	if e.Context == "" {
		return fmt.Errorf("missing context")
	}
	if e.State == "" {
		return fmt.Errorf("missing state")
	}
	if e.Repository.FullName == "" {
		return fmt.Errorf("missing repository")
	}
	return nil
}

func mapState(s string) pg.CheckState {
	switch s {
	case "success", "warning":
		return pg.CheckStateSuccess
	case "failure":
		return pg.CheckStateFailure
	case "error":
		return pg.CheckStateError
	default:
		return pg.CheckStatePending
	}
}

// findEntryForCommit looks up a queue entry by merge branch SHA. This is how
// we correlate a commit status event to a specific PR in the queue.
func findEntryForCommit(ctx context.Context, svc *queue.Service, repoID int64, sha string) *pg.QueueEntry {
	entries, err := svc.ListActiveEntries(ctx, repoID)
	if err != nil {
		slog.Error("failed to list active entries", "error", err)
		return nil
	}

	for i := range entries {
		if entries[i].MergeBranchSha.Valid && entries[i].MergeBranchSha.String == sha {
			return &entries[i]
		}
	}

	return nil
}
