// Package merge handles merge branch creation, cleanup, and startup
// reconciliation for the merge queue.
package merge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Mic92/gitea-mq/internal/forge"
	"github.com/Mic92/gitea-mq/internal/queue"
	"github.com/Mic92/gitea-mq/internal/store/pg"
)

// BranchPrefix is the prefix for temporary merge branches created by gitea-mq.
const BranchPrefix = "gitea-mq/"

// StaleMirrorDescription marks gitea-mq/* mirrored statuses that were reset
// when a PR re-entered testing. The forge has no delete-status API, so we
// overwrite with this sentinel and later flip matching pendings to skipped.
const StaleMirrorDescription = "From a previous merge queue attempt"

func BranchName(prNumber int64) string {
	return fmt.Sprintf("%s%d", BranchPrefix, prNumber)
}

type StartTestingResult struct {
	MergeBranchName string
	MergeBranchSHA  string
	Removed         bool // true if the PR was removed from the queue instead of entering testing
}

// StartTesting creates a merge branch for the head-of-queue PR and
// transitions it to the "testing" state. If the merge conflicts, the PR
// is removed from the queue with automerge cancelled and a comment posted.
func StartTesting(ctx context.Context, f forge.Forge, svc *queue.Service, owner, repo string, repoID int64, entry *pg.QueueEntry, externalURL string) (*StartTestingResult, error) {
	branchName := BranchName(entry.PrNumber)
	targetURL := forge.DashboardPRURL(externalURL, f.Kind(), owner, repo, entry.PrNumber)

	mergeSHA, conflict, err := f.CreateMergeBranch(ctx, owner, repo, entry.TargetBranch, entry.PrHeadSha, branchName)
	if conflict {
		slog.Info("merge conflict", "pr", entry.PrNumber)

		_ = f.CancelAutoMerge(ctx, owner, repo, entry.PrNumber)
		_ = f.SetMQStatus(ctx, owner, repo, entry.PrHeadSha, forge.MQStatus{
			State: pg.CheckStateFailure, Description: "Merge conflict with target branch", TargetURL: targetURL,
		})
		_ = f.Comment(ctx, owner, repo, entry.PrNumber,
			"❌ Removed from merge queue: merge conflict with target branch. Please rebase and re-schedule automerge.")

		if _, err := svc.Dequeue(ctx, repoID, entry.PrNumber); err != nil {
			return nil, fmt.Errorf("dequeue conflicting PR #%d: %w", entry.PrNumber, err)
		}
		return &StartTestingResult{Removed: true}, nil
	}
	if err != nil {
		// Non-conflict failure (e.g. unrelated histories) — surface to the
		// user and remove rather than retry silently.
		slog.Error("merge branch creation failed", "pr", entry.PrNumber, "error", err)

		_ = f.CancelAutoMerge(ctx, owner, repo, entry.PrNumber)
		_ = f.SetMQStatus(ctx, owner, repo, entry.PrHeadSha, forge.MQStatus{
			State: pg.CheckStateError, Description: "Failed to create merge branch", TargetURL: targetURL,
		})
		_ = f.Comment(ctx, owner, repo, entry.PrNumber,
			fmt.Sprintf("❌ Removed from merge queue: failed to create merge branch.\n\n```\n%v\n```", err))

		if _, err := svc.Dequeue(ctx, repoID, entry.PrNumber); err != nil {
			return nil, fmt.Errorf("dequeue PR #%d after merge error: %w", entry.PrNumber, err)
		}
		return &StartTestingResult{Removed: true}, nil
	}

	if err := svc.SetMergeBranch(ctx, repoID, entry.PrNumber, branchName, mergeSHA); err != nil {
		return nil, fmt.Errorf("set merge branch for PR #%d: %w", entry.PrNumber, err)
	}
	if err := svc.UpdateState(ctx, repoID, entry.PrNumber, pg.EntryStateTesting); err != nil {
		return nil, fmt.Errorf("update state to testing for PR #%d: %w", entry.PrNumber, err)
	}

	// Reset stale gitea-mq/* mirrors from a previous attempt so they don't
	// show outdated results while new CI runs.
	clearStaleMirroredStatuses(ctx, f, owner, repo, entry.PrHeadSha)

	_ = f.SetMQStatus(ctx, owner, repo, entry.PrHeadSha, forge.MQStatus{
		State: pg.CheckStatePending, Description: "Testing merge result", TargetURL: targetURL,
	})

	slog.Info("started testing", "pr", entry.PrNumber, "branch", branchName, "sha", mergeSHA)

	return &StartTestingResult{MergeBranchName: branchName, MergeBranchSHA: mergeSHA}, nil
}

func clearStaleMirroredStatuses(ctx context.Context, f forge.Forge, owner, repo, sha string) {
	checks, err := f.GetCheckStates(ctx, owner, repo, sha)
	if err != nil {
		slog.Warn("failed to fetch commit statuses for stale cleanup", "sha", sha, "error", err)
		return
	}
	for ctxName := range checks {
		if !forge.IsOwnContext(ctxName) {
			continue
		}
		_ = f.MirrorCheck(ctx, owner, repo, sha, ctxName, forge.Check{
			State:       pg.CheckStatePending,
			Description: StaleMirrorDescription,
		})
	}
}

func CleanupMergeBranch(ctx context.Context, f forge.Forge, owner, repo string, entry *pg.QueueEntry) {
	if !entry.MergeBranchName.Valid || entry.MergeBranchName.String == "" {
		return
	}
	if err := f.DeleteBranch(ctx, owner, repo, entry.MergeBranchName.String); err != nil {
		slog.Warn("failed to delete merge branch", "branch", entry.MergeBranchName.String, "error", err)
	}
}

// CleanupStaleBranches deletes orphaned gitea-mq/* branches not referenced by
// any active queue entry. Called on startup to clean up after crashes.
func CleanupStaleBranches(ctx context.Context, f forge.Forge, svc *queue.Service, owner, repo string, repoID int64) error {
	activeEntries, err := svc.ListActiveEntries(ctx, repoID)
	if err != nil {
		return fmt.Errorf("list active entries: %w", err)
	}

	activeBranches := make(map[string]bool, len(activeEntries))
	for _, e := range activeEntries {
		if e.MergeBranchName.Valid && e.MergeBranchName.String != "" {
			activeBranches[e.MergeBranchName.String] = true
		}
	}

	branches, err := f.ListBranches(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("list branches: %w", err)
	}

	var deleted int
	for _, b := range branches {
		if !strings.HasPrefix(b, BranchPrefix) {
			continue
		}
		if activeBranches[b] {
			continue
		}

		slog.Info("deleting stale merge branch", "owner", owner, "repo", repo, "branch", b)
		if err := f.DeleteBranch(ctx, owner, repo, b); err != nil {
			slog.Warn("failed to delete stale branch", "branch", b, "error", err)
			continue
		}
		deleted++
	}

	slog.Info("startup merge branch cleanup", "owner", owner, "repo", repo,
		"active_branches", len(activeBranches), "stale_deleted", deleted)
	return nil
}
