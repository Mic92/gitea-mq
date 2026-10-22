// Package webhook implements the HTTP handlers that receive forge webhook
// events: commit statuses are routed to the check monitor, PR-level events
// trigger a reconcile of the repo's poller.
package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Mic92/gitea-mq/internal/forge"
	"github.com/Mic92/gitea-mq/internal/monitor"
	"github.com/Mic92/gitea-mq/internal/queue"
	"github.com/Mic92/gitea-mq/internal/store/pg"
)

// RepoMonitor holds the per-repo deps webhook handlers need to route events.
type RepoMonitor struct {
	Deps *monitor.Deps
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

		switch kind := eventKind(r.Header); {
		case kind == "" || kind == "status":
			// Older Gitea versions omit the event header; treat as status.
		case strings.HasPrefix(kind, "pull_request"):
			handlePREvent(w, body, repos, kind)
			return
		default:
			// Acknowledge everything else so the forge does not mark the
			// hook as failing when it is subscribed to more events.
			slog.Debug("ignoring gitea webhook event", "event", kind)
			w.WriteHeader(http.StatusOK)
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
		if forge.IsOwnContext(event.Context) {
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

		check := forge.Check{
			State:       forge.ParseCheckState(event.State),
			Description: event.Description,
			TargetURL:   event.TargetURL,
		}
		routeCheck(r.Context(), rm, queueSvc, event.SHA, event.Context, check)
		maybeTriggerPoll(rm, check.State)
		w.WriteHeader(http.StatusOK)
	})
}

// eventKind returns the delivery's event name. Forgejo sends its own header
// alongside (or, in future, instead of) the Gitea one.
func eventKind(h http.Header) string {
	for _, k := range []string{"X-Gitea-Event", "X-Forgejo-Event", "X-Gogs-Event"} {
		if v := h.Get(k); v != "" {
			return v
		}
	}
	return ""
}

// prEvent is the subset of Gitea's pull_request* payloads we need. Every
// variant (opened, closed, synchronized, label, review, ...) carries the
// repository.
type prEvent struct {
	Action     string `json:"action"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// handlePREvent reconciles the repo on any PR-level change. Like the GitHub
// handler it does not interpret the action: the poller owns the
// enqueue/dequeue logic and is idempotent, so a spare reconcile is harmless.
func handlePREvent(w http.ResponseWriter, body []byte, repos RepoLookup, kind string) {
	var event prEvent
	if err := json.Unmarshal(body, &event); err != nil {
		slog.Warn("malformed webhook payload", "event", kind, "error", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if event.Repository.FullName == "" {
		http.Error(w, "bad request: missing repository", http.StatusBadRequest)
		return
	}
	repoKey := string(forge.KindGitea) + ":" + event.Repository.FullName
	if rm, ok := repos.LookupMonitor(repoKey); ok && rm.TriggerPoll != nil {
		slog.Debug("pull request webhook, reconciling", "repo", repoKey, "event", kind, "action", event.Action)
		rm.TriggerPoll()
	}
	w.WriteHeader(http.StatusOK)
}

// routeCheck is the shared status/check-run path for both forges: match the
// SHA to a testing queue entry, mirror onto the PR head, and feed the monitor.
// Returning 200 even on internal errors avoids forge retries causing duplicate
// processing.
func routeCheck(ctx context.Context, rm *RepoMonitor, svc *queue.Service, sha, checkCtx string, c forge.Check) {
	// The forge may close the request early; keep processing anyway.
	ctx = context.WithoutCancel(ctx)
	entry := findEntryForCommit(ctx, svc, rm.Deps.RepoID, sha)
	if entry == nil {
		return
	}

	if err := monitor.ApplyCheck(ctx, rm.Deps, entry, checkCtx, c); err != nil {
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
