package gitea

// Timeline comment types emitted by Gitea when a user schedules or cancels
// auto-merge on a PR. These are the canonical signals the Gitea adapter folds
// into forge.PR.AutoMergeEnabled.
const (
	commentTypeAutomergeScheduled = "pull_scheduled_merge"
	commentTypeAutomergeCancelled = "pull_cancel_scheduled_merge"
)

// HasAutomergeScheduled reports whether the most recent automerge-related
// timeline comment indicates auto-merge is currently scheduled. An empty
// timeline (no automerge comments) returns false.
func HasAutomergeScheduled(timeline []TimelineComment) bool {
	for i := len(timeline) - 1; i >= 0; i-- {
		switch timeline[i].Type {
		case commentTypeAutomergeScheduled:
			return true
		case commentTypeAutomergeCancelled:
			return false
		}
	}
	return false
}
