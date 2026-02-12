// Package registry coordinates the lifecycle of managed repos: adding,
// removing, and looking up repos at runtime. It owns per-repo resources
// (poller goroutines, monitor deps) and provides thread-safe access for
// the webhook handler and web dashboard.
package registry

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jogman/gitea-mq/internal/config"
	"github.com/jogman/gitea-mq/internal/gitea"
	"github.com/jogman/gitea-mq/internal/merge"
	"github.com/jogman/gitea-mq/internal/monitor"
	"github.com/jogman/gitea-mq/internal/poller"
	"github.com/jogman/gitea-mq/internal/queue"
	"github.com/jogman/gitea-mq/internal/setup"
	"github.com/jogman/gitea-mq/internal/webhook"
)

// ManagedRepo holds the per-repo state for a managed repository.
type ManagedRepo struct {
	Ref     config.RepoRef
	RepoID  int64
	Monitor *webhook.RepoMonitor
	cancel  context.CancelFunc
}

// Deps holds the shared dependencies the registry needs to initialise repos.
type Deps struct {
	Gitea          gitea.Client
	Queue          *queue.Service
	WebhookURL     string // empty if no external URL configured
	WebhookSecret  string
	ExternalURL    string
	PollInterval   time.Duration
	CheckTimeout   time.Duration
	FallbackChecks []string
	SuccessTimeout time.Duration
}

// RepoRegistry manages the set of active repos. Thread-safe for concurrent
// use by the webhook handler, web dashboard, and discovery loop.
type RepoRegistry struct {
	mu    sync.RWMutex
	repos map[string]*ManagedRepo // keyed by "owner/name"

	parentCtx context.Context
	deps      *Deps
}

// New creates a new RepoRegistry. The parentCtx is used as the parent for
// per-repo contexts (cancelling it stops all pollers).
func New(parentCtx context.Context, deps *Deps) *RepoRegistry {
	return &RepoRegistry{
		repos:     make(map[string]*ManagedRepo),
		parentCtx: parentCtx,
		deps:      deps,
	}
}

// Add registers a repo and starts its poller. If the repo is already managed,
// this is a no-op. Setup (DB registration, branch protection, webhook) runs
// before the repo becomes visible to Lookup/List.
func (r *RepoRegistry) Add(ctx context.Context, ref config.RepoRef) error {
	key := ref.String()

	r.mu.RLock()
	_, exists := r.repos[key]
	r.mu.RUnlock()

	if exists {
		return nil
	}

	// Run setup outside the lock to avoid holding it during API calls.
	if r.deps.WebhookURL == "" {
		if err := setup.EnsureBranchProtection(ctx, r.deps.Gitea, ref.Owner, ref.Name); err != nil {
			slog.Warn("branch protection auto-setup failed", "repo", ref, "error", err)
		}
	} else if err := setup.EnsureRepo(ctx, r.deps.Gitea, ref.Owner, ref.Name, r.deps.WebhookURL, r.deps.WebhookSecret); err != nil {
		slog.Warn("auto-setup failed", "repo", ref, "error", err)
	}

	repo, err := r.deps.Queue.GetOrCreateRepo(ctx, ref.Owner, ref.Name)
	if err != nil {
		return err
	}

	if err := merge.CleanupStaleBranches(ctx, r.deps.Gitea, r.deps.Queue, ref.Owner, ref.Name, repo.ID); err != nil {
		slog.Warn("stale branch cleanup failed", "repo", ref, "error", err)
	}

	monDeps := &monitor.Deps{
		Gitea:          r.deps.Gitea,
		Queue:          r.deps.Queue,
		Owner:          ref.Owner,
		Repo:           ref.Name,
		RepoID:         repo.ID,
		ExternalURL:    r.deps.ExternalURL,
		CheckTimeout:   r.deps.CheckTimeout,
		FallbackChecks: r.deps.FallbackChecks,
	}

	pollerCtx, cancel := context.WithCancel(r.parentCtx)

	managed := &ManagedRepo{
		Ref:    ref,
		RepoID: repo.ID,
		Monitor: &webhook.RepoMonitor{
			Deps:   monDeps,
			RepoID: repo.ID,
		},
		cancel: cancel,
	}

	// Start poller goroutine.
	pollerDeps := &poller.Deps{
		Gitea:          r.deps.Gitea,
		Queue:          r.deps.Queue,
		RepoID:         repo.ID,
		Owner:          ref.Owner,
		Repo:           ref.Name,
		ExternalURL:    r.deps.ExternalURL,
		SuccessTimeout: r.deps.SuccessTimeout,
	}
	go poller.Run(pollerCtx, pollerDeps, r.deps.PollInterval)

	// Make visible only after setup is complete.
	r.mu.Lock()
	// Double-check: another goroutine may have added it while we were setting up.
	if _, exists := r.repos[key]; !exists {
		r.repos[key] = managed
	} else {
		// Another goroutine won the race — cancel our duplicate poller.
		cancel()
	}
	r.mu.Unlock()

	return nil
}

// Remove stops a repo's poller, cleans up merge branches and DB entries,
// and removes the repo from the registry. No-op if the repo is not managed.
func (r *RepoRegistry) Remove(ref config.RepoRef) {
	key := ref.String()

	r.mu.Lock()
	managed, exists := r.repos[key]
	if exists {
		delete(r.repos, key)
	}
	r.mu.Unlock()

	if !exists {
		return
	}

	// Cancel the poller first so it stops making new API calls.
	managed.cancel()

	// Clean up merge branches and DB entries using a background context
	// since the per-repo context is now cancelled.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	entries, err := r.deps.Queue.ListActiveEntries(ctx, managed.RepoID)
	if err != nil {
		slog.Warn("failed to list entries for cleanup", "repo", key, "error", err)
	} else {
		for _, entry := range entries {
			merge.CleanupMergeBranch(ctx, r.deps.Gitea, ref.Owner, ref.Name, &entry)
		}
	}

	if err := r.deps.Queue.DequeueAll(ctx, managed.RepoID); err != nil {
		slog.Warn("failed to dequeue entries on removal", "repo", key, "error", err)
	}

	slog.Info("removed repo from registry", "repo", key)
}

// Lookup returns the ManagedRepo for a given "owner/name" key, or nil if
// not managed. Used by the webhook handler.
func (r *RepoRegistry) Lookup(fullName string) (*ManagedRepo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	m, ok := r.repos[fullName]
	return m, ok
}

// LookupMonitor returns the RepoMonitor for a given "owner/name" key.
// Implements webhook.RepoLookup.
func (r *RepoRegistry) LookupMonitor(fullName string) (*webhook.RepoMonitor, bool) {
	m, ok := r.Lookup(fullName)
	if !ok {
		return nil, false
	}
	return m.Monitor, true
}

// List returns a snapshot of all currently managed repo refs.
// Used by the web dashboard.
func (r *RepoRegistry) List() []config.RepoRef {
	r.mu.RLock()
	defer r.mu.RUnlock()

	refs := make([]config.RepoRef, 0, len(r.repos))
	for _, m := range r.repos {
		refs = append(refs, m.Ref)
	}
	return refs
}

// Contains returns true if the given "owner/name" is currently managed.
func (r *RepoRegistry) Contains(fullName string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	_, ok := r.repos[fullName]
	return ok
}

// Keys returns the set of all managed repo keys ("owner/name").
func (r *RepoRegistry) Keys() map[string]struct{} {
	r.mu.RLock()
	defer r.mu.RUnlock()

	keys := make(map[string]struct{}, len(r.repos))
	for k := range r.repos {
		keys[k] = struct{}{}
	}
	return keys
}
