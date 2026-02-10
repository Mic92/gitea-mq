// Package poller discovers PRs with automerge scheduled by polling the
// Gitea API timeline for pull_scheduled_merge / pull_cancel_scheduled_merge
// comment types.
package poller

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jogman/gitea-mq/internal/gitea"
	"github.com/jogman/gitea-mq/internal/queue"
	"github.com/jogman/gitea-mq/internal/store/pg"
)

// Deps holds the dependencies the poller needs. Using a struct instead of
// individual fields makes testing straightforward — tests inject mocks.
type Deps struct {
	Gitea  gitea.Client
	Queue  *queue.Service
	RepoID int64
	Owner  string
	Repo   string
	// SuccessTimeout is how long a PR can sit in "success" state without
	// being merged before we consider automerge failed.
	SuccessTimeout time.Duration
}

// PollResult describes what happened during a single poll cycle.
type PollResult struct {
	Enqueued []int64 // PR numbers newly enqueued
	Dequeued []int64 // PR numbers removed from queue
	Advanced []int64 // PR numbers that caused queue advancement
	Errors   []error // non-fatal errors encountered
	Paused   bool    // true if Gitea was unreachable
}

// PollOnce runs a single poll cycle for one repository:
//
//  1. Fetch all open PRs
//  2. For each open PR: check timeline → enqueue if automerge scheduled
//  3. For each queued PR: check if automerge cancelled → dequeue
//  4. For each queued PR: check if merged → remove + advance
//  5. For each queued PR: check if head SHA changed → remove + cancel automerge
//  6. For each queued PR: check if closed → remove + cleanup
//  7. For each queued PR: check if target branch changed → remove
//  8. For head-of-queue in success state: check if still open too long → cancel
func PollOnce(ctx context.Context, deps *Deps) (*PollResult, error) {
	result := &PollResult{}

	// Step 1: Fetch all open PRs.
	openPRs, err := deps.Gitea.ListOpenPRs(ctx, deps.Owner, deps.Repo)
	if err != nil {
		return &PollResult{Paused: true, Errors: []error{err}}, nil
	}

	// Build a lookup of open PRs by index.
	openPRMap := make(map[int64]*gitea.PR, len(openPRs))
	for i := range openPRs {
		openPRMap[openPRs[i].Index] = &openPRs[i]
	}

	// Step 2: For each open PR, check automerge state.
	for _, pr := range openPRs {
		timeline, err := deps.Gitea.GetPRTimeline(ctx, deps.Owner, deps.Repo, pr.Index)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("get timeline for PR #%d: %w", pr.Index, err))
			continue
		}

		if !HasAutomergeScheduled(timeline) {
			continue
		}

		// Check if already queued.
		existing, err := deps.Queue.GetEntry(ctx, deps.RepoID, pr.Index)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("check queue for PR #%d: %w", pr.Index, err))
			continue
		}
		if existing != nil {
			continue // Already queued, no-op.
		}

		// Enqueue the PR.
		headSHA := ""
		if pr.Head != nil {
			headSHA = pr.Head.Sha
		}
		targetBranch := ""
		if pr.Base != nil {
			targetBranch = pr.Base.Ref
		}

		enqResult, err := deps.Queue.Enqueue(ctx, deps.RepoID, pr.Index, headSHA, targetBranch)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("enqueue PR #%d: %w", pr.Index, err))
			continue
		}

		if enqResult.IsNew {
			// Set gitea-mq pending status on the PR's head commit.
			desc := fmt.Sprintf("Queued (position #%d)", enqResult.Position)
			if err := deps.Gitea.CreateCommitStatus(ctx, deps.Owner, deps.Repo, headSHA, gitea.CommitStatus{
				Context:     "gitea-mq",
				State:       "pending",
				Description: desc,
			}); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("set pending status for PR #%d: %w", pr.Index, err))
			}

			result.Enqueued = append(result.Enqueued, pr.Index)
			slog.Info("enqueued PR from automerge detection", "pr", pr.Index, "position", enqResult.Position)
		}
	}

	// Step 3-8: Check all queued entries for state changes.
	// We need to get all active entries for this repo.
	activeEntries, err := deps.Queue.ListActiveEntries(ctx, deps.RepoID)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("list active entries: %w", err))
		return result, nil
	}

	for _, entry := range activeEntries {
		pr, isOpen := openPRMap[entry.PrNumber]

		// Step 6: Closed PR detection — PR no longer in open PRs list.
		if !isOpen {
			// Fetch actual PR state to distinguish closed vs merged.
			fullPR, err := deps.Gitea.GetPR(ctx, deps.Owner, deps.Repo, entry.PrNumber)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("get PR #%d: %w", entry.PrNumber, err))
				continue
			}

			if fullPR.HasMerged {
				// Step 4: Merged PR — remove + advance.
				dqResult, err := deps.Queue.Dequeue(ctx, deps.RepoID, entry.PrNumber)
				if err != nil {
					result.Errors = append(result.Errors, fmt.Errorf("dequeue merged PR #%d: %w", entry.PrNumber, err))
					continue
				}
				if dqResult.WasHead {
					// Clean up merge branch if present.
					if entry.MergeBranchName.Valid && entry.MergeBranchName.String != "" {
						if err := deps.Gitea.DeleteBranch(ctx, deps.Owner, deps.Repo, entry.MergeBranchName.String); err != nil {
							result.Errors = append(result.Errors, fmt.Errorf("delete merge branch for PR #%d: %w", entry.PrNumber, err))
						}
					}
					result.Advanced = append(result.Advanced, entry.PrNumber)
				}
				result.Dequeued = append(result.Dequeued, entry.PrNumber)
				slog.Info("removed merged PR from queue", "pr", entry.PrNumber)
				continue
			}

			// Closed but not merged — silently remove.
			dqResult, err := deps.Queue.Dequeue(ctx, deps.RepoID, entry.PrNumber)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("dequeue closed PR #%d: %w", entry.PrNumber, err))
				continue
			}
			if dqResult.WasHead && entry.MergeBranchName.Valid && entry.MergeBranchName.String != "" {
				if err := deps.Gitea.DeleteBranch(ctx, deps.Owner, deps.Repo, entry.MergeBranchName.String); err != nil {
					result.Errors = append(result.Errors, fmt.Errorf("delete merge branch for closed PR #%d: %w", entry.PrNumber, err))
				}
			}
			result.Dequeued = append(result.Dequeued, entry.PrNumber)
			slog.Info("removed closed PR from queue", "pr", entry.PrNumber)
			continue
		}

		// Step 7: Target branch change detection.
		if pr.Base != nil && pr.Base.Ref != entry.TargetBranch {
			dqResult, err := deps.Queue.Dequeue(ctx, deps.RepoID, entry.PrNumber)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("dequeue retargeted PR #%d: %w", entry.PrNumber, err))
				continue
			}
			_ = deps.Gitea.CancelAutoMerge(ctx, deps.Owner, deps.Repo, entry.PrNumber)
			_ = deps.Gitea.CreateComment(ctx, deps.Owner, deps.Repo, entry.PrNumber,
				fmt.Sprintf("⚠️ Removed from merge queue: target branch changed from `%s` to `%s`. Please re-schedule automerge.", entry.TargetBranch, pr.Base.Ref))
			if dqResult.WasHead && entry.MergeBranchName.Valid && entry.MergeBranchName.String != "" {
				_ = deps.Gitea.DeleteBranch(ctx, deps.Owner, deps.Repo, entry.MergeBranchName.String)
			}
			result.Dequeued = append(result.Dequeued, entry.PrNumber)
			slog.Info("removed retargeted PR from queue", "pr", entry.PrNumber, "old_branch", entry.TargetBranch, "new_branch", pr.Base.Ref)
			continue
		}

		// Step 5: Head SHA changed → new push detection.
		if pr.Head != nil && pr.Head.Sha != entry.PrHeadSha {
			dqResult, err := deps.Queue.Dequeue(ctx, deps.RepoID, entry.PrNumber)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("dequeue pushed PR #%d: %w", entry.PrNumber, err))
				continue
			}
			_ = deps.Gitea.CancelAutoMerge(ctx, deps.Owner, deps.Repo, entry.PrNumber)
			_ = deps.Gitea.CreateComment(ctx, deps.Owner, deps.Repo, entry.PrNumber,
				"⚠️ Removed from merge queue: new commits were pushed. Please re-schedule automerge.")
			if dqResult.WasHead && entry.MergeBranchName.Valid && entry.MergeBranchName.String != "" {
				_ = deps.Gitea.DeleteBranch(ctx, deps.Owner, deps.Repo, entry.MergeBranchName.String)
			}
			result.Dequeued = append(result.Dequeued, entry.PrNumber)
			result.Advanced = append(result.Advanced, entry.PrNumber)
			slog.Info("removed PR due to new push", "pr", entry.PrNumber)
			continue
		}

		// Step 3: Automerge cancellation detection.
		timeline, err := deps.Gitea.GetPRTimeline(ctx, deps.Owner, deps.Repo, entry.PrNumber)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("get timeline for queued PR #%d: %w", entry.PrNumber, err))
			continue
		}

		if !HasAutomergeScheduled(timeline) {
			dqResult, err := deps.Queue.Dequeue(ctx, deps.RepoID, entry.PrNumber)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("dequeue cancelled PR #%d: %w", entry.PrNumber, err))
				continue
			}
			if dqResult.WasHead && entry.MergeBranchName.Valid && entry.MergeBranchName.String != "" {
				_ = deps.Gitea.DeleteBranch(ctx, deps.Owner, deps.Repo, entry.MergeBranchName.String)
			}
			result.Dequeued = append(result.Dequeued, entry.PrNumber)
			slog.Info("removed PR due to automerge cancellation", "pr", entry.PrNumber)
			continue
		}

		// Step 8: Success-but-not-merged timeout detection.
		if entry.State == pg.EntryStateSuccess && deps.SuccessTimeout > 0 {
			if entry.CompletedAt.Valid {
				completedTime := entry.CompletedAt.Time
				if time.Since(completedTime) > deps.SuccessTimeout {
					// PR has been in success state too long — automerge probably failed.
					_ = deps.Gitea.CancelAutoMerge(ctx, deps.Owner, deps.Repo, entry.PrNumber)
					_ = deps.Gitea.CreateCommitStatus(ctx, deps.Owner, deps.Repo, entry.PrHeadSha, gitea.CommitStatus{
						Context:     "gitea-mq",
						State:       "error",
						Description: "Automerge did not complete in time",
					})
					_ = deps.Gitea.CreateComment(ctx, deps.Owner, deps.Repo, entry.PrNumber,
						"⚠️ Removed from merge queue: PR was marked as ready to merge but Gitea did not merge it in time. This may indicate a branch protection issue.")
					_ = deps.Queue.SetError(ctx, deps.RepoID, entry.PrNumber, "automerge did not complete in time")
					if _, err := deps.Queue.Dequeue(ctx, deps.RepoID, entry.PrNumber); err != nil {
						result.Errors = append(result.Errors, fmt.Errorf("dequeue timed-out PR #%d: %w", entry.PrNumber, err))
					}
					if entry.MergeBranchName.Valid && entry.MergeBranchName.String != "" {
						_ = deps.Gitea.DeleteBranch(ctx, deps.Owner, deps.Repo, entry.MergeBranchName.String)
					}
					result.Dequeued = append(result.Dequeued, entry.PrNumber)
					result.Advanced = append(result.Advanced, entry.PrNumber)
					slog.Info("removed PR due to success-but-not-merged timeout", "pr", entry.PrNumber)
				}
			}
		}
	}

	return result, nil
}

// Run starts the polling loop. It runs PollOnce on every tick and stops when
// ctx is cancelled. The first poll happens immediately (no initial delay).
func Run(ctx context.Context, deps *Deps, interval time.Duration) {
	slog.Info("poller started", "owner", deps.Owner, "repo", deps.Repo, "interval", interval)

	// Run immediately on startup.
	if _, err := PollOnce(ctx, deps); err != nil {
		slog.Error("poll error", "owner", deps.Owner, "repo", deps.Repo, "error", err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("poller stopped", "owner", deps.Owner, "repo", deps.Repo)

			return
		case <-ticker.C:
			result, err := PollOnce(ctx, deps)
			if err != nil {
				slog.Error("poll error", "owner", deps.Owner, "repo", deps.Repo, "error", err)

				continue
			}

			if result.Paused {
				slog.Warn("Gitea unavailable, pausing", "owner", deps.Owner, "repo", deps.Repo)
			}

			for _, e := range result.Errors {
				slog.Warn("poll issue", "owner", deps.Owner, "repo", deps.Repo, "error", e)
			}
		}
	}
}
