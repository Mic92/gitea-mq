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

	"github.com/Mic92/gitea-mq/internal/forge"
	"github.com/Mic92/gitea-mq/internal/merge"
	"github.com/Mic92/gitea-mq/internal/monitor"
	"github.com/Mic92/gitea-mq/internal/poller"
	"github.com/Mic92/gitea-mq/internal/queue"
	"github.com/Mic92/gitea-mq/internal/webhook"
)

type ManagedRepo struct {
	Ref     forge.RepoRef
	RepoID  int64
	Monitor *webhook.RepoMonitor
	cancel  context.CancelFunc
}

type Deps struct {
	Forges         *forge.Set
	Queue          *queue.Service
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
	repos map[string]*ManagedRepo // keyed by forge.RepoRef.String()

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

// Add registers a repo and starts its poller. No-op if already managed.
// Setup (forge auto-setup, DB registration, stale-branch cleanup) runs before
// the repo becomes visible to Lookup/List.
func (r *RepoRegistry) Add(ctx context.Context, ref forge.RepoRef) error {
	key := ref.String()

	r.mu.RLock()
	_, exists := r.repos[key]
	r.mu.RUnlock()

	if exists {
		return nil
	}

	f, err := r.deps.Forges.For(ref)
	if err != nil {
		return err
	}

	if err := f.EnsureRepoSetup(ctx, ref.Owner, ref.Name, forge.SetupConfig{
		ExternalURL:   r.deps.ExternalURL,
		WebhookSecret: r.deps.WebhookSecret,
	}); err != nil {
		slog.Warn("auto-setup failed", "repo", key, "error", err)
	}

	repo, err := r.deps.Queue.GetOrCreateRepo(ctx, string(ref.Forge), ref.Owner, ref.Name)
	if err != nil {
		return err
	}

	if err := merge.CleanupStaleBranches(ctx, f, r.deps.Queue, ref.Owner, ref.Name, repo.ID); err != nil {
		slog.Warn("stale branch cleanup failed", "repo", key, "error", err)
	}

	monDeps := &monitor.Deps{
		Forge:          f,
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

	pollerDeps := &poller.Deps{
		Forge:          f,
		Queue:          r.deps.Queue,
		RepoID:         repo.ID,
		Owner:          ref.Owner,
		Repo:           ref.Name,
		ExternalURL:    r.deps.ExternalURL,
		FallbackChecks: r.deps.FallbackChecks,
		SuccessTimeout: r.deps.SuccessTimeout,
	}
	go poller.Run(pollerCtx, pollerDeps, r.deps.PollInterval)

	r.mu.Lock()
	if _, exists := r.repos[key]; !exists {
		r.repos[key] = managed
	} else {
		// Another Add won the race; drop our duplicate poller.
		cancel()
	}
	r.mu.Unlock()

	return nil
}

// Remove stops a repo's poller, cleans up merge branches and DB entries,
// and removes the repo from the registry. No-op if the repo is not managed.
func (r *RepoRegistry) Remove(ref forge.RepoRef) {
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

	managed.cancel()

	f, err := r.deps.Forges.For(ref)
	if err != nil {
		slog.Warn("no forge for repo on removal", "repo", key, "error", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	entries, err := r.deps.Queue.ListActiveEntries(ctx, managed.RepoID)
	if err != nil {
		slog.Warn("failed to list entries for cleanup", "repo", key, "error", err)
	} else {
		for _, entry := range entries {
			merge.CleanupMergeBranch(ctx, f, ref.Owner, ref.Name, &entry)
		}
	}

	if err := r.deps.Queue.DequeueAll(ctx, managed.RepoID); err != nil {
		slog.Warn("failed to dequeue entries on removal", "repo", key, "error", err)
	}

	slog.Info("removed repo from registry", "repo", key)
}

// Lookup returns the ManagedRepo for a given "<forge>:<owner>/<name>" key.
func (r *RepoRegistry) Lookup(key string) (*ManagedRepo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	m, ok := r.repos[key]
	return m, ok
}

// LookupMonitor implements webhook.RepoLookup.
func (r *RepoRegistry) LookupMonitor(key string) (*webhook.RepoMonitor, bool) {
	m, ok := r.Lookup(key)
	if !ok {
		return nil, false
	}
	return m.Monitor, true
}

// List returns a snapshot of all currently managed repo refs.
func (r *RepoRegistry) List() []forge.RepoRef {
	r.mu.RLock()
	defer r.mu.RUnlock()

	refs := make([]forge.RepoRef, 0, len(r.repos))
	for _, m := range r.repos {
		refs = append(refs, m.Ref)
	}
	return refs
}

func (r *RepoRegistry) Contains(key string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	_, ok := r.repos[key]
	return ok
}

func (r *RepoRegistry) Keys() map[string]struct{} {
	r.mu.RLock()
	defer r.mu.RUnlock()

	keys := make(map[string]struct{}, len(r.repos))
	for k := range r.repos {
		keys[k] = struct{}{}
	}
	return keys
}
