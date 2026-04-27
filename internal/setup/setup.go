// Package setup is a thin shim over internal/gitea kept until callers
// migrate to forge.Forge.EnsureRepoSetup.
package setup

import (
	"context"

	"github.com/Mic92/gitea-mq/internal/gitea"
)

func EnsureBranchProtection(ctx context.Context, client gitea.Client, owner, repo string) error {
	return gitea.EnsureBranchProtection(ctx, client, owner, repo)
}

func EnsureWebhook(ctx context.Context, client gitea.Client, owner, repo, webhookURL, secret string) error {
	return gitea.EnsureWebhook(ctx, client, owner, repo, webhookURL, secret)
}

func EnsureRepo(ctx context.Context, client gitea.Client, owner, repo, webhookURL, secret string) error {
	if err := gitea.EnsureBranchProtection(ctx, client, owner, repo); err != nil {
		return err
	}
	return gitea.EnsureWebhook(ctx, client, owner, repo, webhookURL, secret)
}
