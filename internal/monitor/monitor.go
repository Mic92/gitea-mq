// Package monitor evaluates commit status checks on merge branches and
// decides when the head-of-queue PR passes, fails, or times out.
package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jogman/gitea-mq/internal/gitea"
	"github.com/jogman/gitea-mq/internal/merge"
	"github.com/jogman/gitea-mq/internal/queue"
	"github.com/jogman/gitea-mq/internal/store/pg"
)

// Deps holds the dependencies the monitor needs.
type Deps struct {
	Gitea          gitea.Client
	Queue          *queue.Service
	Owner          string
	Repo           string
	RepoID         int64
	CheckTimeout   time.Duration
	FallbackChecks []string // from GITEA_MQ_REQUIRED_CHECKS
}

// CheckResult describes the outcome of evaluating checks for an entry.
type CheckResult int

const (
	// CheckWaiting means not all required checks have reported yet.
	CheckWaiting CheckResult = iota
	// CheckSuccess means all required checks passed.
	CheckSuccess
	// CheckFailure means at least one required check failed.
	CheckFailure
)

// ResolveRequiredChecks determines which check contexts are required for a
// given target branch. It queries branch protection first, falls back to
// config, and finally falls back to "any single success suffices".
//
// Returns the list of required check context names. An empty list means
// "any single success status is sufficient".
func ResolveRequiredChecks(ctx context.Context, giteaClient gitea.Client, owner, repo, targetBranch string, fallback []string) ([]string, error) {
	bp, err := giteaClient.GetBranchProtection(ctx, owner, repo, targetBranch)
	if err != nil {
		return nil, fmt.Errorf("get branch protection for %s: %w", targetBranch, err)
	}

	if bp != nil && bp.EnableStatusCheck && len(bp.StatusCheckContexts) > 0 {
		// Filter out gitea-mq to avoid circular dependency.
		var checks []string
		for _, c := range bp.StatusCheckContexts {
			if c != "gitea-mq" {
				checks = append(checks, c)
			}
		}

		if len(checks) > 0 {
			return checks, nil
		}
	}

	// Fall back to config-defined checks.
	if len(fallback) > 0 {
		return fallback, nil
	}

	// No config — any single success suffices.
	return nil, nil
}

// EvaluateChecks compares recorded check statuses against required checks.
// Returns CheckSuccess if all required pass, CheckFailure if any required
// failed, CheckWaiting otherwise.
//
// If requiredChecks is empty, any single success status is sufficient.
func EvaluateChecks(statuses []pg.CheckStatus, requiredChecks []string) (CheckResult, string) {
	if len(requiredChecks) == 0 {
		// Any single success suffices.
		for _, s := range statuses {
			if s.State == pg.CheckStateSuccess {
				return CheckSuccess, ""
			}
		}

		return CheckWaiting, ""
	}

	// Build a lookup of latest status per context.
	statusMap := make(map[string]pg.CheckState, len(statuses))
	for _, s := range statuses {
		statusMap[s.Context] = s.State
	}

	for _, req := range requiredChecks {
		state, ok := statusMap[req]
		if !ok {
			return CheckWaiting, "" // Not yet reported.
		}

		switch state {
		case pg.CheckStateFailure, pg.CheckStateError:
			return CheckFailure, req
		case pg.CheckStatePending:
			return CheckWaiting, ""
		case pg.CheckStateSuccess:
			continue
		}
	}

	return CheckSuccess, ""
}

// CheckTimeout returns true if the entry has exceeded the check timeout.
func CheckTimeout(entry *pg.QueueEntry, timeout time.Duration) bool {
	if !entry.TestingStartedAt.Valid {
		return false
	}

	return time.Since(entry.TestingStartedAt.Time) > timeout
}

// HandleSuccess processes a successful check evaluation for the head-of-queue.
// Sets gitea-mq to success, deletes the merge branch, transitions to success state.
// Does NOT advance — the poller confirms the PR is actually merged first.
func HandleSuccess(ctx context.Context, deps *Deps, entry *pg.QueueEntry) error {
	slog.Info("all checks passed", "pr", entry.PrNumber)

	// Set gitea-mq commit status to success on the PR's head commit.
	if err := deps.Gitea.CreateCommitStatus(ctx, deps.Owner, deps.Repo, entry.PrHeadSha, gitea.CommitStatus{
		Context:     "gitea-mq",
		State:       "success",
		Description: "Merge queue passed",
	}); err != nil {
		return fmt.Errorf("set success status for PR #%d: %w", entry.PrNumber, err)
	}

	// Delete the merge branch — CI is done, no longer needed.
	merge.CleanupMergeBranch(ctx, deps.Gitea, deps.Owner, deps.Repo, entry)

	// Transition to success state. The poller will advance once the PR is merged.
	if err := deps.Queue.UpdateState(ctx, deps.RepoID, entry.PrNumber, pg.EntryStateSuccess); err != nil {
		return fmt.Errorf("update state to success for PR #%d: %w", entry.PrNumber, err)
	}

	return nil
}

// HandleFailure processes a check failure for the head-of-queue.
// Sets gitea-mq to failure, cancels automerge, posts comment, deletes merge branch, advances.
func HandleFailure(ctx context.Context, deps *Deps, entry *pg.QueueEntry, failedCheck string) error {
	slog.Info("check failed", "pr", entry.PrNumber, "check", failedCheck)

	desc := fmt.Sprintf("Check failed: %s", failedCheck)

	if err := deps.Gitea.CreateCommitStatus(ctx, deps.Owner, deps.Repo, entry.PrHeadSha, gitea.CommitStatus{
		Context:     "gitea-mq",
		State:       "failure",
		Description: desc,
	}); err != nil {
		slog.Warn("failed to set failure status", "pr", entry.PrNumber, "error", err)
	}

	if err := deps.Gitea.CancelAutoMerge(ctx, deps.Owner, deps.Repo, entry.PrNumber); err != nil {
		slog.Warn("failed to cancel automerge", "pr", entry.PrNumber, "error", err)
	}

	comment := fmt.Sprintf("❌ Removed from merge queue: %s", desc)
	if err := deps.Gitea.CreateComment(ctx, deps.Owner, deps.Repo, entry.PrNumber, comment); err != nil {
		slog.Warn("failed to post failure comment", "pr", entry.PrNumber, "error", err)
	}

	merge.CleanupMergeBranch(ctx, deps.Gitea, deps.Owner, deps.Repo, entry)

	if err := deps.Queue.UpdateState(ctx, deps.RepoID, entry.PrNumber, pg.EntryStateFailed); err != nil {
		slog.Warn("failed to update state to failed", "pr", entry.PrNumber, "error", err)
	}

	// Advance to next PR.
	if _, err := deps.Queue.Advance(ctx, deps.RepoID, entry.TargetBranch); err != nil {
		return fmt.Errorf("advance queue after failure of PR #%d: %w", entry.PrNumber, err)
	}

	return nil
}

// HandleTimeout processes a check timeout for the head-of-queue.
func HandleTimeout(ctx context.Context, deps *Deps, entry *pg.QueueEntry) error {
	slog.Info("check timeout exceeded", "pr", entry.PrNumber)

	if err := deps.Gitea.CreateCommitStatus(ctx, deps.Owner, deps.Repo, entry.PrHeadSha, gitea.CommitStatus{
		Context:     "gitea-mq",
		State:       "error",
		Description: "Check timeout exceeded",
	}); err != nil {
		slog.Warn("failed to set error status", "pr", entry.PrNumber, "error", err)
	}

	if err := deps.Gitea.CancelAutoMerge(ctx, deps.Owner, deps.Repo, entry.PrNumber); err != nil {
		slog.Warn("failed to cancel automerge", "pr", entry.PrNumber, "error", err)
	}

	comment := "⏰ Removed from merge queue: check timeout exceeded. Required checks did not complete in time."
	if err := deps.Gitea.CreateComment(ctx, deps.Owner, deps.Repo, entry.PrNumber, comment); err != nil {
		slog.Warn("failed to post timeout comment", "pr", entry.PrNumber, "error", err)
	}

	merge.CleanupMergeBranch(ctx, deps.Gitea, deps.Owner, deps.Repo, entry)

	if err := deps.Queue.UpdateState(ctx, deps.RepoID, entry.PrNumber, pg.EntryStateFailed); err != nil {
		slog.Warn("failed to update state to failed", "pr", entry.PrNumber, "error", err)
	}

	if _, err := deps.Queue.Advance(ctx, deps.RepoID, entry.TargetBranch); err != nil {
		return fmt.Errorf("advance queue after timeout of PR #%d: %w", entry.PrNumber, err)
	}

	return nil
}

// ProcessCheckStatus is the main entry point called when a webhook delivers
// a commit status event for a merge branch. It records the status, evaluates
// checks, and triggers success/failure handling as appropriate.
func ProcessCheckStatus(ctx context.Context, deps *Deps, entry *pg.QueueEntry, checkContext string, checkState pg.CheckState) error {
	// Record the check status (latest wins — upsert).
	if err := deps.Queue.SaveCheckStatus(ctx, entry.ID, checkContext, checkState); err != nil {
		return fmt.Errorf("save check status for PR #%d: %w", entry.PrNumber, err)
	}

	// Resolve required checks for this target branch.
	requiredChecks, err := ResolveRequiredChecks(ctx, deps.Gitea, deps.Owner, deps.Repo, entry.TargetBranch, deps.FallbackChecks)
	if err != nil {
		return fmt.Errorf("resolve required checks: %w", err)
	}

	// Get all recorded statuses for this entry.
	statuses, err := deps.Queue.GetCheckStatuses(ctx, entry.ID)
	if err != nil {
		return fmt.Errorf("get check statuses for PR #%d: %w", entry.PrNumber, err)
	}

	// Evaluate.
	result, failedCheck := EvaluateChecks(statuses, requiredChecks)

	switch result {
	case CheckSuccess:
		return HandleSuccess(ctx, deps, entry)
	case CheckFailure:
		return HandleFailure(ctx, deps, entry, failedCheck)
	case CheckWaiting:
		// Still waiting for more checks. Check timeout.
		if CheckTimeout(entry, deps.CheckTimeout) {
			return HandleTimeout(ctx, deps, entry)
		}
	}

	return nil
}
