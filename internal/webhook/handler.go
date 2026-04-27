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

	"github.com/Mic92/gitea-mq/internal/forge"
	"github.com/Mic92/gitea-mq/internal/gitea"
	"github.com/Mic92/gitea-mq/internal/monitor"
	"github.com/Mic92/gitea-mq/internal/queue"
	"github.com/Mic92/gitea-mq/internal/store/pg"
)

// RepoMonitor holds the per-repo deps webhook handlers need to route events.
type RepoMonitor struct {
	Deps   *monitor.Deps
	RepoID int64
	// TriggerPoll fires an immediate reconcile of the repo's poller. Used
	// for PR-level webhooks (auto-merge toggle, close, push) where the
	// poller already owns the correct enqueue/dequeue logic.
	TriggerPoll func()
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

		// Gitea payloads identify repos as owner/name; the registry keys by
		// forge:owner/name.
		repoKey := string(forge.KindGitea) + ":" + event.Repository.FullName
		rm, ok := repos.LookupMonitor(repoKey)
		if !ok {
			slog.Debug("webhook for unmanaged repo", "repo", repoKey)
			w.WriteHeader(http.StatusOK)
			return
		}

		routeCheck(r.Context(), rm, queueSvc, event.SHA, event.Context, gitea.MapState(event.State), event.State, event.Description, event.TargetURL)
		w.WriteHeader(http.StatusOK)
	})
}

// routeCheck is the shared status/check-run path for both forges: match the
// SHA to a testing queue entry, mirror onto the PR head, and feed the monitor.
// Returning 200 even on internal errors avoids forge retries causing duplicate
// processing.
func routeCheck(ctx context.Context, rm *RepoMonitor, svc *queue.Service, sha, checkCtx string, state pg.CheckState, rawState, desc, targetURL string) {
	entry := findEntryForCommit(ctx, svc, rm.RepoID, sha)
	if entry == nil {
		return
	}

	mirrorCtx := "gitea-mq/" + checkCtx
	if err := rm.Deps.Forge.MirrorCheck(ctx, rm.Deps.Owner, rm.Deps.Repo, entry.PrHeadSha,
		mirrorCtx, rawState, desc, targetURL); err != nil {
		slog.Warn("failed to mirror status to PR head", "pr", entry.PrNumber, "context", mirrorCtx, "err", err)
	}

	if err := monitor.ProcessCheckStatus(ctx, rm.Deps, entry, checkCtx, state, targetURL); err != nil {
		slog.Error("failed to process check status", "pr", entry.PrNumber, "err", err)
	}
}

// statusEvent is the subset of Gitea's commit_status webhook payload we need.
type statusEvent struct {
	SHA         string `json:"sha"`
	Context     string `json:"context"`
	State       string `json:"state"` // "pending", "success", "failure", "error"
	Description string `json:"description"`
	TargetURL   string `json:"target_url"`
	Repository  struct {
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
