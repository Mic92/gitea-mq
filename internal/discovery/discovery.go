// Package discovery implements periodic topic-based repo discovery from the
// Gitea API. It reconciles the discovered set with the registry, adding new
// repos and removing ones that lost the topic.
package discovery

import (
	"context"
	"log/slog"
	"time"

	"github.com/Mic92/gitea-mq/internal/forge"
	"github.com/Mic92/gitea-mq/internal/gitea"
	"github.com/Mic92/gitea-mq/internal/registry"
)

type Deps struct {
	Gitea         gitea.Client
	Registry      *registry.RepoRegistry
	Topic         string
	ExplicitRepos []forge.RepoRef
}

// DiscoverOnce runs a single discovery cycle: searches repos by topic,
// filters by admin access, merges with explicit repos, and reconciles
// the registry.
func DiscoverOnce(ctx context.Context, deps *Deps) error {
	repos, err := deps.Gitea.SearchReposByTopic(ctx, deps.Topic)
	if err != nil {
		slog.Warn("discovery: failed to search repos by topic", "error", err)
		return err
	}

	desired := make(map[string]forge.RepoRef)

	for _, repo := range repos {
		if !repo.Permissions.Admin {
			slog.Debug("discovery: skipping repo without admin access",
				"repo", repo.FullName)
			continue
		}

		ref := forge.RepoRef{Forge: forge.KindGitea, Owner: repo.Owner.Login, Name: repo.Name}
		desired[ref.String()] = ref
	}

	for _, ref := range deps.ExplicitRepos {
		desired[ref.String()] = ref
	}

	// Reconcile: add new repos.
	for key, ref := range desired {
		if !deps.Registry.Contains(key) {
			slog.Info("discovery: adding repo", "repo", key)
			if err := deps.Registry.Add(ctx, ref); err != nil {
				slog.Warn("discovery: failed to add repo", "repo", key, "error", err)
			}
		}
	}

	// Reconcile: remove repos that lost the topic (but not explicit ones).
	explicitSet := make(map[string]struct{}, len(deps.ExplicitRepos))
	for _, ref := range deps.ExplicitRepos {
		explicitSet[ref.String()] = struct{}{}
	}

	for key := range deps.Registry.Keys() {
		if _, inDesired := desired[key]; !inDesired {
			if _, isExplicit := explicitSet[key]; isExplicit {
				continue
			}
			slog.Info("discovery: removing repo", "repo", key)
			ref, ok := forge.ParseRepoRef(key)
			if ok {
				deps.Registry.Remove(ref)
			}
		}
	}

	slog.Info("discovery: cycle complete", "managed", len(desired))
	return nil
}

// Run starts the background discovery loop, repeating at the given interval.
// The caller is responsible for the initial DiscoverOnce call.
// Stops when ctx is cancelled.
func Run(ctx context.Context, deps *Deps, interval time.Duration) {
	slog.Info("discovery loop started", "topic", deps.Topic, "interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("discovery loop stopped")
			return
		case <-ticker.C:
			if err := DiscoverOnce(ctx, deps); err != nil {
				slog.Error("discovery error", "error", err)
			}
		}
	}
}
