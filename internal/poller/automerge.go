// Package poller discovers PRs with automerge scheduled by polling the
// Gitea API timeline for pull_scheduled_merge / pull_cancel_scheduled_merge
// comment types.
package poller

import "github.com/jogman/gitea-mq/internal/gitea"

// automergeCommentType is the timeline comment type for scheduling automerge.
const automergeCommentType = "pull_scheduled_merge"

// cancelAutomergeCommentType is the timeline comment type for cancelling automerge.
const cancelAutomergeCommentType = "pull_cancel_scheduled_merge"

// HasAutomergeScheduled returns true if the most recent automerge-related
// timeline comment indicates that automerge is currently scheduled.
// An empty timeline (no automerge comments) returns false.
func HasAutomergeScheduled(timeline []gitea.TimelineComment) bool {
	// Walk backwards to find the latest automerge-related comment.
	for i := len(timeline) - 1; i >= 0; i-- {
		switch timeline[i].Type {
		case automergeCommentType:
			return true
		case cancelAutomergeCommentType:
			return false
		}
	}

	return false
}
