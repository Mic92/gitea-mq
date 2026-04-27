// Package monitor evaluates commit status checks on merge branches and
// decides when the head-of-queue PR passes, fails, or times out.
package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Mic92/gitea-mq/internal/forge"
	"github.com/Mic92/gitea-mq/internal/merge"
	"github.com/Mic92/gitea-mq/internal/queue"
	"github.com/Mic92/gitea-mq/internal/store/pg"
)

type Deps struct {
	Forge          forge.Forge
	Queue          *queue.Service
	Owner          string
	Repo           string
	RepoID         int64
	ExternalURL    string
	CheckTimeout   time.Duration
	FallbackChecks []string // from GITEA_MQ_REQUIRED_CHECKS
}

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
// target branch: forge-reported required checks first, falling back to config,
// then to "any single success suffices" (empty list).
func ResolveRequiredChecks(ctx context.Context, f forge.Forge, owner, repo, targetBranch string, fallback []string) ([]string, error) {
	checks, err := f.GetRequiredChecks(ctx, owner, repo, targetBranch)
	if err != nil {
		return nil, fmt.Errorf("get required checks for %s: %w", targetBranch, err)
	}
	if len(checks) > 0 {
		return checks, nil
	}
	return fallback, nil
}

// EvaluateChecks compares recorded check statuses against required checks.
// Returns CheckSuccess if all required pass, CheckFailure if any required
// failed, CheckWaiting otherwise. The second string is the failed check name,
// and the third is its target URL (both empty when result is not CheckFailure).
//
// If requiredChecks is empty, any single success status is sufficient.
func EvaluateChecks(statuses []pg.CheckStatus, requiredChecks []string) (CheckResult, string, string) {
	if len(requiredChecks) == 0 {
		for _, s := range statuses {
			if s.State == pg.CheckStateSuccess {
				return CheckSuccess, "", ""
			}
		}
		return CheckWaiting, "", ""
	}

	statusMap := make(map[string]pg.CheckStatus, len(statuses))
	for _, s := range statuses {
		statusMap[s.Context] = s
	}

	for _, req := range requiredChecks {
		cs, ok := statusMap[req]
		if !ok {
			return CheckWaiting, "", ""
		}
		switch cs.State {
		case pg.CheckStateFailure, pg.CheckStateError:
			return CheckFailure, req, cs.TargetUrl
		case pg.CheckStatePending:
			return CheckWaiting, "", ""
		case pg.CheckStateSuccess:
			continue
		default:
			return CheckWaiting, "", ""
		}
	}

	return CheckSuccess, "", ""
}

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

	targetURL := forge.DashboardPRURL(deps.ExternalURL, deps.Forge.Kind(), deps.Owner, deps.Repo, entry.PrNumber)

	if err := deps.Forge.SetMQStatus(ctx, deps.Owner, deps.Repo, entry.PrHeadSha, forge.MQStatus{
		State: pg.CheckStateSuccess, Description: "Merge queue passed", TargetURL: targetURL,
	}); err != nil {
		return fmt.Errorf("set success status for PR #%d: %w", entry.PrNumber, err)
	}

	// Flip leftover stale-pending mirrors so the overall PR status is green.
	skipPendingMirroredStatuses(ctx, deps.Forge, deps.Owner, deps.Repo, entry.PrHeadSha)

	merge.CleanupMergeBranch(ctx, deps.Forge, deps.Owner, deps.Repo, entry)

	if err := deps.Queue.UpdateState(ctx, deps.RepoID, entry.PrNumber, pg.EntryStateSuccess); err != nil {
		return fmt.Errorf("update state to success for PR #%d: %w", entry.PrNumber, err)
	}

	return nil
}

// skipPendingMirroredStatuses sets gitea-mq/* mirrors that are still pending
// with the StaleMirrorDescription to skipped, so cleared mirrors from a
// previous attempt don't block the green result.
func skipPendingMirroredStatuses(ctx context.Context, f forge.Forge, owner, repo, sha string) {
	checks, err := f.GetCheckStates(ctx, owner, repo, sha)
	if err != nil {
		slog.Warn("failed to fetch commit statuses for skip cleanup", "sha", sha, "error", err)
		return
	}
	for ctxName, c := range checks {
		if !strings.HasPrefix(ctxName, merge.BranchPrefix) {
			continue
		}
		if c.State != pg.CheckStatePending || c.Description != merge.StaleMirrorDescription {
			continue
		}
		_ = f.MirrorCheck(ctx, owner, repo, sha, ctxName, "skipped", merge.StaleMirrorDescription, "")
	}
}

func HandleFailure(ctx context.Context, deps *Deps, entry *pg.QueueEntry, failedCheck, targetURL string) error {
	slog.Info("check failed", "pr", entry.PrNumber, "check", failedCheck)

	desc := fmt.Sprintf("Check failed: %s", failedCheck)
	checkRef := failedCheck
	if targetURL != "" {
		checkRef = fmt.Sprintf("[%s](%s)", failedCheck, targetURL)
	}

	return removeFromQueue(ctx, deps, entry, pg.CheckStateFailure, desc,
		fmt.Sprintf("❌ Removed from merge queue: Check failed: %s", checkRef))
}

func HandleTimeout(ctx context.Context, deps *Deps, entry *pg.QueueEntry) error {
	slog.Info("check timeout exceeded", "pr", entry.PrNumber)

	return removeFromQueue(ctx, deps, entry, pg.CheckStateError, "Check timeout exceeded",
		"⏰ Removed from merge queue: check timeout exceeded. Required checks did not complete in time.")
}

func removeFromQueue(ctx context.Context, deps *Deps, entry *pg.QueueEntry, statusState pg.CheckState, statusDesc, comment string) error {
	targetURL := forge.DashboardPRURL(deps.ExternalURL, deps.Forge.Kind(), deps.Owner, deps.Repo, entry.PrNumber)
	if err := deps.Forge.SetMQStatus(ctx, deps.Owner, deps.Repo, entry.PrHeadSha, forge.MQStatus{
		State: statusState, Description: statusDesc, TargetURL: targetURL,
	}); err != nil {
		slog.Warn("failed to set status", "pr", entry.PrNumber, "error", err)
	}

	if err := deps.Forge.CancelAutoMerge(ctx, deps.Owner, deps.Repo, entry.PrNumber); err != nil {
		slog.Warn("failed to cancel automerge", "pr", entry.PrNumber, "error", err)
	}

	if err := deps.Forge.Comment(ctx, deps.Owner, deps.Repo, entry.PrNumber, comment); err != nil {
		slog.Warn("failed to post comment", "pr", entry.PrNumber, "error", err)
	}

	merge.CleanupMergeBranch(ctx, deps.Forge, deps.Owner, deps.Repo, entry)

	if err := deps.Queue.UpdateState(ctx, deps.RepoID, entry.PrNumber, pg.EntryStateFailed); err != nil {
		slog.Warn("failed to update state to failed", "pr", entry.PrNumber, "error", err)
	}

	if _, err := deps.Queue.Advance(ctx, deps.RepoID, entry.TargetBranch); err != nil {
		return fmt.Errorf("advance queue after removing PR #%d: %w", entry.PrNumber, err)
	}

	return nil
}

// ProcessCheckStatus is the main entry point called when a webhook delivers
// a commit status event for a merge branch. It records the status, evaluates
// checks, and triggers success/failure handling as appropriate.
func ProcessCheckStatus(ctx context.Context, deps *Deps, entry *pg.QueueEntry, checkContext string, checkState pg.CheckState, targetURL string) error {
	if err := deps.Queue.SaveCheckStatus(ctx, entry.ID, checkContext, checkState, targetURL); err != nil {
		return fmt.Errorf("save check status for PR #%d: %w", entry.PrNumber, err)
	}

	requiredChecks, err := ResolveRequiredChecks(ctx, deps.Forge, deps.Owner, deps.Repo, entry.TargetBranch, deps.FallbackChecks)
	if err != nil {
		return fmt.Errorf("resolve required checks: %w", err)
	}

	statuses, err := deps.Queue.GetCheckStatuses(ctx, entry.ID)
	if err != nil {
		return fmt.Errorf("get check statuses for PR #%d: %w", entry.PrNumber, err)
	}

	result, failedCheck, failedURL := EvaluateChecks(statuses, requiredChecks)

	switch result {
	case CheckSuccess:
		return HandleSuccess(ctx, deps, entry)
	case CheckFailure:
		return HandleFailure(ctx, deps, entry, failedCheck, failedURL)
	case CheckWaiting:
		if CheckTimeout(entry, deps.CheckTimeout) {
			return HandleTimeout(ctx, deps, entry)
		}
	}

	return nil
}
