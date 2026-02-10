// Package merge handles merge branch creation, cleanup, and startup
// reconciliation for the merge queue.
package merge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jogman/gitea-mq/internal/gitea"
	"github.com/jogman/gitea-mq/internal/queue"
	"github.com/jogman/gitea-mq/internal/store/pg"
)

// StartTestingResult describes the outcome of starting testing for a PR.
type StartTestingResult struct {
	MergeBranchName string
	MergeBranchSHA  string
	Conflict        bool // true if the merge failed due to conflicts
}

// StartTesting creates a merge branch for the head-of-queue PR and
// transitions it to the "testing" state. If the merge conflicts, the PR
// is removed from the queue with automerge cancelled and a comment posted.
func StartTesting(ctx context.Context, giteaClient gitea.Client, svc *queue.Service, owner, repo string, repoID int64, entry *pg.QueueEntry) (*StartTestingResult, error) {
	branchName := fmt.Sprintf("mq/%d", entry.PrNumber)

	mergeResult, err := giteaClient.MergeBranches(ctx, owner, repo, entry.TargetBranch, entry.PrHeadSha, branchName)
	if err != nil {
		if gitea.IsMergeConflict(err) {
			slog.Info("merge conflict", "pr", entry.PrNumber)

			// Cancel automerge and notify.
			_ = giteaClient.CancelAutoMerge(ctx, owner, repo, entry.PrNumber)
			_ = giteaClient.CreateCommitStatus(ctx, owner, repo, entry.PrHeadSha,
				gitea.MQStatus("failure", "Merge conflict with target branch"))
			_ = giteaClient.CreateComment(ctx, owner, repo, entry.PrNumber,
				"❌ Removed from merge queue: merge conflict with target branch. Please rebase and re-schedule automerge.")

			// Remove from queue and advance.
			if _, err := svc.Dequeue(ctx, repoID, entry.PrNumber); err != nil {
				return nil, fmt.Errorf("dequeue conflicting PR #%d: %w", entry.PrNumber, err)
			}

			return &StartTestingResult{Conflict: true}, nil
		}

		return nil, fmt.Errorf("create merge branch for PR #%d: %w", entry.PrNumber, err)
	}

	// Record merge branch and transition to testing.
	if err := svc.SetMergeBranch(ctx, repoID, entry.PrNumber, branchName, mergeResult.SHA); err != nil {
		return nil, fmt.Errorf("set merge branch for PR #%d: %w", entry.PrNumber, err)
	}

	if err := svc.UpdateState(ctx, repoID, entry.PrNumber, pg.EntryStateTesting); err != nil {
		return nil, fmt.Errorf("update state to testing for PR #%d: %w", entry.PrNumber, err)
	}

	// Update the pending status to indicate testing.
	_ = giteaClient.CreateCommitStatus(ctx, owner, repo, entry.PrHeadSha,
		gitea.MQStatus("pending", "Testing merge result"))

	slog.Info("started testing", "pr", entry.PrNumber, "branch", branchName, "sha", mergeResult.SHA)

	return &StartTestingResult{
		MergeBranchName: branchName,
		MergeBranchSHA:  mergeResult.SHA,
	}, nil
}

// CleanupMergeBranch deletes a merge branch if it exists.
func CleanupMergeBranch(ctx context.Context, giteaClient gitea.Client, owner, repo string, entry *pg.QueueEntry) {
	if !entry.MergeBranchName.Valid || entry.MergeBranchName.String == "" {
		return
	}

	if err := giteaClient.DeleteBranch(ctx, owner, repo, entry.MergeBranchName.String); err != nil {
		slog.Warn("failed to delete merge branch", "branch", entry.MergeBranchName.String, "error", err)
	}
}

// CleanupStaleBranches scans for orphaned mq/* branches and deletes them.
// Called on startup to clean up after crashes. A branch is considered stale
// if its name starts with "mq/" but is not referenced by any active queue
// entry.
func CleanupStaleBranches(ctx context.Context, giteaClient gitea.Client, svc *queue.Service, owner, repo string, repoID int64) error {
	// Get all active entries to know which merge branches are legitimate.
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

	// List all branches and find orphaned mq/* ones.
	branches, err := giteaClient.ListBranches(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("list branches: %w", err)
	}

	var deleted int
	for _, b := range branches {
		if !strings.HasPrefix(b.Name, "mq/") {
			continue
		}

		if activeBranches[b.Name] {
			continue
		}

		slog.Info("deleting stale merge branch", "owner", owner, "repo", repo, "branch", b.Name)

		if err := giteaClient.DeleteBranch(ctx, owner, repo, b.Name); err != nil {
			slog.Warn("failed to delete stale branch", "branch", b.Name, "error", err)

			continue
		}

		deleted++
	}

	slog.Info("startup merge branch cleanup", "owner", owner, "repo", repo,
		"active_branches", len(activeBranches), "stale_deleted", deleted)

	return nil
}
