// Package discovery reconciles the desired set of managed repos against the
// registry. Each forge contributes a Source; reconciliation is scoped per
// forge so one backend being unreachable does not evict another's repos.
package discovery

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/Mic92/gitea-mq/internal/forge"
	"github.com/Mic92/gitea-mq/internal/registry"
)

// Source enumerates repos a forge wants managed.
type Source struct {
	Kind forge.Kind
	List func(ctx context.Context) ([]forge.RepoRef, error)
}

type Deps struct {
	Sources       []Source
	Registry      *registry.RepoRegistry
	ExplicitRepos []forge.RepoRef
	// Trigger fires an immediate cycle in addition to the interval, e.g. on
	// an installation webhook.
	Trigger <-chan struct{}
}

func DiscoverOnce(ctx context.Context, deps *Deps) {
	explicit := make(map[string]forge.RepoRef, len(deps.ExplicitRepos))
	for _, r := range deps.ExplicitRepos {
		explicit[r.String()] = r
	}
	// Explicit repos are added regardless of whether their forge has a
	// dynamic source so a Gitea-only config without topic discovery still
	// brings repos up.
	addNew(ctx, deps.Registry, explicit)

	for _, src := range deps.Sources {
		refs, err := src.List(ctx)
		if err != nil {
			// Do not reconcile this forge: removing repos because the API is
			// down would amplify an outage into queue loss.
			slog.Warn("discovery: source failed; keeping current set", "forge", src.Kind, "err", err)
			continue
		}
		desired := map[string]forge.RepoRef{}
		for _, r := range refs {
			desired[r.String()] = r
		}
		addNew(ctx, deps.Registry, desired)

		prefix := string(src.Kind) + ":"
		for key := range deps.Registry.Keys() {
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			if _, ok := desired[key]; ok {
				continue
			}
			if _, ok := explicit[key]; ok {
				continue
			}
			slog.Info("discovery: removing repo", "repo", key)
			if ref, ok := forge.ParseRepoRef(key); ok {
				deps.Registry.Remove(ref)
			}
		}
		slog.Info("discovery: reconciled", "forge", src.Kind, "managed", len(desired))
	}
}

func addNew(ctx context.Context, reg *registry.RepoRegistry, desired map[string]forge.RepoRef) {
	for key, ref := range desired {
		if reg.Contains(key) {
			continue
		}
		slog.Info("discovery: adding repo", "repo", key)
		if err := reg.Add(ctx, ref); err != nil {
			slog.Warn("discovery: failed to add repo", "repo", key, "err", err)
		}
	}
}

func Run(ctx context.Context, deps *Deps, interval time.Duration) {
	slog.Info("discovery loop started", "interval", interval, "sources", len(deps.Sources))

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("discovery loop stopped")
			return
		case <-ticker.C:
			DiscoverOnce(ctx, deps)
		case <-deps.Trigger:
			DiscoverOnce(ctx, deps)
		}
	}
}
