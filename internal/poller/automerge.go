// Package poller discovers PRs with automerge scheduled by polling the
// Gitea API timeline for pull_scheduled_merge / pull_cancel_scheduled_merge
// comment types.
package poller

import "github.com/Mic92/gitea-mq/internal/gitea"

// HasAutomergeScheduled remains until the poller migrates to forge.Forge
// (which folds this into ListAutoMergePRs).
func HasAutomergeScheduled(timeline []gitea.TimelineComment) bool {
	return gitea.HasAutomergeScheduled(timeline)
}
