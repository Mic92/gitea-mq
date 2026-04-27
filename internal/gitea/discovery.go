package gitea

import (
	"context"
	"log/slog"

	"github.com/Mic92/gitea-mq/internal/forge"
)

// TopicSource lists repos carrying the given topic that the token has admin
// access to. Admin is required because EnsureRepoSetup mutates branch
// protection and webhooks.
func TopicSource(c Client, topic string) func(context.Context) ([]forge.RepoRef, error) {
	return func(ctx context.Context) ([]forge.RepoRef, error) {
		repos, err := c.SearchReposByTopic(ctx, topic)
		if err != nil {
			return nil, err
		}
		var out []forge.RepoRef
		for _, r := range repos {
			if !r.Permissions.Admin {
				slog.Debug("discovery: skipping repo without admin access", "repo", r.FullName)
				continue
			}
			out = append(out, forge.RepoRef{Forge: forge.KindGitea, Owner: r.Owner.Login, Name: r.Name})
		}
		return out, nil
	}
}
