package gitea

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
)

// EnsureBranchProtection adds `gitea-mq` to every branch-protection rule's
// required status checks if missing. With no rules present it logs a warning
// and returns nil — gitea-mq can still run, the user just won't get gating.
func EnsureBranchProtection(ctx context.Context, client Client, owner, repo string) error {
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

		newContexts := append(slices.Clone(bp.StatusCheckContexts), "gitea-mq")
		enable := true
		opts := EditBranchProtectionOpts{
			EnableStatusCheck:   &enable,
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

// EnsureWebhook creates a `status`-event webhook pointing at webhookURL unless
// one already exists with that URL.
func EnsureWebhook(ctx context.Context, client Client, owner, repo, webhookURL, secret string) error {
	hooks, err := client.ListWebhooks(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("list webhooks for %s/%s: %w", owner, repo, err)
	}

	for _, h := range hooks {
		if h.Config["url"] == webhookURL {
			slog.Debug("webhook already exists",
				"owner", owner, "repo", repo, "url", webhookURL)
			return nil
		}
	}

	opts := CreateWebhookOpts{
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
