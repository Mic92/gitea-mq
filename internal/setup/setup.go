// Package setup auto-configures Gitea repos for use with gitea-mq:
// ensures `gitea-mq` is a required status check in branch protection
// and ensures a webhook exists for commit_status events.
package setup

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/jogman/gitea-mq/internal/gitea"
)

// EnsureBranchProtection checks all branch protection rules for a repo and
// adds `gitea-mq` to the required status checks if missing.
// If no branch protection rules exist, it logs a warning and returns.
func EnsureBranchProtection(ctx context.Context, client gitea.Client, owner, repo string) error {
	bps, err := client.ListBranchProtections(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("list branch protections for %s/%s: %w", owner, repo, err)
	}

	if len(bps) == 0 {
		slog.Warn("no branch protection rules found, gitea-mq requires branch protection with status checks",
			"owner", owner, "repo", repo)
		return nil
	}

	for _, bp := range bps {
		if slices.Contains(bp.StatusCheckContexts, "gitea-mq") {
			slog.Debug("gitea-mq already in required checks",
				"owner", owner, "repo", repo, "rule", bp.RuleName)
			continue
		}

		// Add gitea-mq to the status checks.
		newContexts := append(bp.StatusCheckContexts, "gitea-mq")
		enableStatusCheck := true
		opts := gitea.EditBranchProtectionOpts{
			EnableStatusCheck:   &enableStatusCheck,
			StatusCheckContexts: newContexts,
		}

		if err := client.EditBranchProtection(ctx, owner, repo, bp.RuleName, opts); err != nil {
			return fmt.Errorf("add gitea-mq to branch protection %q in %s/%s: %w",
				bp.RuleName, owner, repo, err)
		}

		slog.Info("added gitea-mq to required status checks",
			"owner", owner, "repo", repo, "rule", bp.RuleName)
	}

	return nil
}

// EnsureWebhook checks if a webhook for commit_status events already exists
// pointing at the given URL and creates one if not.
func EnsureWebhook(ctx context.Context, client gitea.Client, owner, repo, webhookURL, secret string) error {
	hooks, err := client.ListWebhooks(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("list webhooks for %s/%s: %w", owner, repo, err)
	}

	// Check if a matching webhook already exists.
	for _, h := range hooks {
		if h.Config["url"] == webhookURL {
			slog.Debug("webhook already exists",
				"owner", owner, "repo", repo, "url", webhookURL)
			return nil
		}
	}

	opts := gitea.CreateWebhookOpts{
		Type:   "gitea",
		Events: []string{"status"},
		Active: true,
		Config: map[string]string{
			"url":          webhookURL,
			"content_type": "json",
			"secret":       secret,
		},
	}

	if err := client.CreateWebhook(ctx, owner, repo, opts); err != nil {
		return fmt.Errorf("create webhook for %s/%s: %w", owner, repo, err)
	}

	slog.Info("created webhook", "owner", owner, "repo", repo, "url", webhookURL)

	return nil
}

// EnsureRepo runs both EnsureBranchProtection and EnsureWebhook for a repo.
func EnsureRepo(ctx context.Context, client gitea.Client, owner, repo, webhookURL, secret string) error {
	if err := EnsureBranchProtection(ctx, client, owner, repo); err != nil {
		return err
	}

	return EnsureWebhook(ctx, client, owner, repo, webhookURL, secret)
}
