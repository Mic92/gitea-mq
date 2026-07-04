// Package poller reconciles the queue with the forge: it discovers PRs with
// auto-merge enabled, enqueues them once their own CI is green, and removes
// queued PRs that were merged/closed/retargeted/pushed/cancelled.
package poller

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Mic92/gitea-mq/internal/batch"
	"github.com/Mic92/gitea-mq/internal/forge"
	"github.com/Mic92/gitea-mq/internal/merge"
	"github.com/Mic92/gitea-mq/internal/monitor"
	"github.com/Mic92/gitea-mq/internal/queue"
	"github.com/Mic92/gitea-mq/internal/store/pg"
)

type Deps struct {
	Forge  forge.Forge
	Queue  *queue.Service
	RepoID int64
	Owner  string
	Repo   string
	// Trigger fires an immediate reconcile, used by webhooks so PR-level
	// events are reflected without waiting for the poll interval.
	Trigger <-chan struct{}
	// ExternalURL is the dashboard base URL for status target_url.
	ExternalURL string
	// FallbackChecks gates enqueue when branch protection lists none.
	FallbackChecks []string
	// SuccessTimeout is how long a PR may sit in "success" without merging
	// before we assume the forge's auto-merge failed.
	SuccessTimeout time.Duration
	// SkipQueueIfUpToDate skips the merge-branch CI run when the head PR
	// already contains the target tip: its own green CI covered the same tree.
	SkipQueueIfUpToDate bool
	// CheckTimeout is how long a PR may sit in "testing" without a check
	// status before we abandon it. Without this, a CI server that loses a
	// build (e.g. a restart) leaves the head-of-queue stuck forever since
	// the timeout in monitor.ProcessCheckStatus only runs on webhooks.
	CheckTimeout time.Duration
	// Batch enables bors-style batching when non-nil. The legacy single-PR
	// path is taken when nil so BATCH_MAX=1 stays byte-for-byte unchanged.
	Batch *batch.Engine
}

type PollResult struct {
	Enqueued []int64
	Dequeued []int64
	Advanced []int64
	Errors   []error
	Paused   bool // forge unreachable
}

type removeOpts struct {
	cancelAutomerge bool
	comment         string
	advance         bool
	logMsg          string
	logAttrs        []any
}

func removePR(ctx context.Context, deps *Deps, result *PollResult, entry *pg.QueueEntry, opts removeOpts) error {
	dqResult, err := deps.Queue.Dequeue(ctx, deps.RepoID, entry.PrNumber)
	if err != nil {
		return err
	}

	if opts.cancelAutomerge {
		_ = deps.Forge.CancelAutoMerge(ctx, deps.Owner, deps.Repo, entry.PrNumber)
	}
	if opts.comment != "" {
		_ = deps.Forge.Comment(ctx, deps.Owner, deps.Repo, entry.PrNumber, opts.comment)
	}
	// The batch engine owns gitea-mq/batch/<id>; deleting it here can race a
	// concurrent Build and cause MergeInto to fail with wrong attribution.
	if dqResult.WasHead && !entry.ActiveBatchID.Valid {
		merge.CleanupMergeBranch(ctx, deps.Forge, deps.Owner, deps.Repo, entry)
	}

	result.Dequeued = append(result.Dequeued, entry.PrNumber)
	if opts.advance {
		result.Advanced = append(result.Advanced, entry.PrNumber)
	}

	slog.Info(opts.logMsg, append([]any{"pr", entry.PrNumber}, opts.logAttrs...)...)
	return nil
}

// prChecksGreen reports whether the PR's own head-commit checks are passing.
// True when all required checks pass, or when no CI is configured at all.
func prChecksGreen(ctx context.Context, deps *Deps, pr *forge.PR) (bool, error) {
	requiredChecks, err := monitor.ResolveRequiredChecks(ctx, deps.Forge, deps.Owner, deps.Repo, pr.BaseBranch, deps.FallbackChecks)
	if err != nil {
		return false, fmt.Errorf("resolve required checks for PR #%d: %w", pr.Number, err)
	}

	checks, err := deps.Forge.GetCheckStates(ctx, deps.Owner, deps.Repo, pr.HeadSHA)
	if err != nil {
		return false, fmt.Errorf("get check states for PR #%d: %w", pr.Number, err)
	}

	// gitea-mq/* mirrors are our own output, not external CI.
	var externalStatuses []pg.CheckStatus
	for ctxName, c := range checks {
		if forge.IsOwnContext(ctxName) {
			continue
		}
		externalStatuses = append(externalStatuses, pg.CheckStatus{Context: ctxName, State: c.State})
	}

	if len(requiredChecks) == 0 && len(externalStatuses) == 0 {
		return true, nil
	}

	res, _, _ := monitor.EvaluateChecks(externalStatuses, requiredChecks)
	return res == monitor.CheckSuccess, nil
}

// PollOnce runs a single reconcile cycle for one repository.
func PollOnce(ctx context.Context, deps *Deps) (*PollResult, error) {
	result := &PollResult{}

	openPRs, err := deps.Forge.ListOpenPRs(ctx, deps.Owner, deps.Repo)
	if err != nil {
		return &PollResult{Paused: true, Errors: []error{err}}, nil
	}

	openPRMap := make(map[int64]*forge.PR, len(openPRs))
	for i := range openPRs {
		openPRMap[openPRs[i].Number] = &openPRs[i]
	}

	enqueueAutoMergePRs(ctx, deps, result, openPRs)
	reconcileEntries(ctx, deps, result, openPRMap)
	startQueuedHeads(ctx, deps, result)
	pollMergeBranchChecks(ctx, deps, result)

	return result, nil
}

// monitorDeps adapts the poller's Deps into the monitor's Deps.
func monitorDeps(deps *Deps) *monitor.Deps {
	m := &monitor.Deps{
		Forge:          deps.Forge,
		Queue:          deps.Queue,
		Owner:          deps.Owner,
		Repo:           deps.Repo,
		RepoID:         deps.RepoID,
		ExternalURL:    deps.ExternalURL,
		CheckTimeout:   deps.CheckTimeout,
		FallbackChecks: deps.FallbackChecks,
	}
	if deps.Batch.Enabled() {
		m.Batch = deps.Batch
	}
	return m
}

// pollMergeBranchChecks feeds each testing entry's merge-branch commit
// statuses to the monitor, covering forges without a status webhook and
// missed webhook deliveries.
func pollMergeBranchChecks(ctx context.Context, deps *Deps, result *PollResult) {
	activeEntries, err := deps.Queue.ListActiveEntries(ctx, deps.RepoID)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("list active entries for check poll: %w", err))
		return
	}

	mDeps := monitorDeps(deps)

	// Batches are driven from the row's BranchSha so we never feed checks for
	// a stale entry SHA back into the engine after a rebuild. This loop is
	// also the timeout safety net when CI never reports.
	if deps.Batch.Enabled() {
		bs, err := deps.Queue.ListLiveBatches(ctx, deps.RepoID)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("list live batches: %w", err))
		}
		for i := range bs {
			b := &bs[i]
			if b.State != pg.BatchStateTesting || len(b.CurrentIds) == 0 {
				continue
			}
			if batch.TimedOut(b, deps.CheckTimeout) {
				if err := deps.Batch.HandleTimeout(ctx, b.TargetBranch, b.ID); err != nil {
					result.Errors = append(result.Errors, fmt.Errorf("batch #%d timeout: %w", b.ID, err))
				}
				continue
			}
			rep, err := deps.Queue.GetEntriesByIDs(ctx, b.CurrentIds[:1])
			if err != nil || len(rep) == 0 {
				continue
			}
			// rep may have been re-stamped by a concurrent rebuild; the SHA we
			// fetch checks for is the one HandleCheck must guard on.
			rep[0].MergeBranchSha = b.BranchSha
			checks, err := deps.Forge.GetCheckStates(ctx, deps.Owner, deps.Repo, b.BranchSha.String)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("get batch #%d checks: %w", b.ID, err))
				continue
			}
			for ctxName, c := range checks {
				if forge.IsOwnContext(ctxName) {
					continue
				}
				if err := monitor.ApplyCheck(ctx, mDeps, &rep[0], ctxName, c); err != nil {
					result.Errors = append(result.Errors, fmt.Errorf("process batch #%d check: %w", b.ID, err))
				}
			}
		}
	}

	for i := range activeEntries {
		entry := &activeEntries[i]
		// Batch members are handled by the per-batch loop above; the
		// snapshot's merge_branch_sha may be stale here.
		if entry.ActiveBatchID.Valid {
			continue
		}
		if entry.State != pg.EntryStateTesting || !entry.MergeBranchSha.Valid {
			continue
		}

		checks, err := deps.Forge.GetCheckStates(ctx, deps.Owner, deps.Repo, entry.MergeBranchSha.String)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("get merge branch checks for PR #%d: %w", entry.PrNumber, err))
			continue
		}

		for ctxName, c := range checks {
			// gitea-mq/* mirrors are our own output, not external CI.
			if forge.IsOwnContext(ctxName) {
				continue
			}
			if err := monitor.ApplyCheck(ctx, mDeps, entry, ctxName, c); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("process merge branch check for PR #%d: %w", entry.PrNumber, err))
			}
		}
	}
}

// enqueueAutoMergePRs adds open PRs that have auto-merge enabled and green CI
// to the queue if they are not already tracked.
func enqueueAutoMergePRs(ctx context.Context, deps *Deps, result *PollResult, openPRs []forge.PR) {
	for i := range openPRs {
		pr := &openPRs[i]
		if !pr.AutoMergeEnabled {
			continue
		}

		existing, err := deps.Queue.GetEntry(ctx, deps.RepoID, pr.Number)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("check queue for PR #%d: %w", pr.Number, err))
			continue
		}
		if existing != nil {
			continue
		}

		green, err := prChecksGreen(ctx, deps, pr)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("check CI status for PR #%d: %w", pr.Number, err))
			continue
		}
		if !green {
			continue
		}

		enqResult, err := deps.Queue.Enqueue(ctx, deps.RepoID, pr.Number, pr.HeadSHA, pr.BaseBranch)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("enqueue PR #%d: %w", pr.Number, err))
			continue
		}

		if enqResult.IsNew {
			desc := fmt.Sprintf("Queued (position #%d)", enqResult.Position)
			targetURL := forge.DashboardPRURL(deps.ExternalURL, deps.Forge.Kind(), deps.Owner, deps.Repo, pr.Number)
			if err := deps.Forge.SetMQStatus(ctx, deps.Owner, deps.Repo, pr.HeadSHA, forge.MQStatus{
				State: pg.CheckStatePending, Description: desc, TargetURL: targetURL,
			}); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("set pending status for PR #%d: %w", pr.Number, err))
			}

			result.Enqueued = append(result.Enqueued, pr.Number)
			slog.Info("enqueued PR from automerge detection", "pr", pr.Number, "position", enqResult.Position)
		}
	}
}

// reconcileEntries removes queue entries whose PR was merged, closed,
// retargeted, pushed to, or had auto-merge cancelled.
func reconcileEntries(ctx context.Context, deps *Deps, result *PollResult, openPRMap map[int64]*forge.PR) {
	activeEntries, err := deps.Queue.ListActiveEntries(ctx, deps.RepoID)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("list active entries: %w", err))
		return
	}

	for _, entry := range activeEntries {
		pr, isOpen := openPRMap[entry.PrNumber]
		if !isOpen {
			// PR no longer in the open list: fetch directly to learn merged/closed.
			fullPR, err := deps.Forge.GetPR(ctx, deps.Owner, deps.Repo, entry.PrNumber)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("get PR #%d: %w", entry.PrNumber, err))
				continue
			}
			pr = fullPR
		}

		checks := []struct {
			when  bool
			label string
			opts  removeOpts
		}{
			{
				when:  !isOpen && pr.Merged,
				label: "merged",
				opts:  removeOpts{advance: true, logMsg: "removed merged PR from queue"},
			},
			{
				when:  !isOpen,
				label: "closed",
				opts:  removeOpts{logMsg: "removed closed PR from queue"},
			},
			{
				when:  pr.BaseBranch != "" && pr.BaseBranch != entry.TargetBranch,
				label: "retargeted",
				opts: removeOpts{
					cancelAutomerge: true,
					comment:         fmt.Sprintf("⚠️ Removed from merge queue: target branch changed from `%s` to `%s`. Please re-schedule automerge.", entry.TargetBranch, pr.BaseBranch),
					logMsg:          "removed retargeted PR from queue",
					logAttrs:        []any{"old_branch", entry.TargetBranch, "new_branch", pr.BaseBranch},
				},
			},
			{
				when:  pr.HeadSHA != "" && pr.HeadSHA != entry.PrHeadSha,
				label: "pushed",
				opts: removeOpts{
					cancelAutomerge: true,
					comment:         "⚠️ Removed from merge queue: new commits were pushed. Please re-schedule automerge.",
					advance:         true,
					logMsg:          "removed PR due to new push",
				},
			},
			{
				when:  !pr.AutoMergeEnabled,
				label: "cancelled",
				opts:  removeOpts{logMsg: "removed PR due to automerge cancellation"},
			},
		}

		matched := false
		for _, c := range checks {
			if !c.when {
				continue
			}
			if err := removePR(ctx, deps, result, &entry, c.opts); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("dequeue %s PR #%d: %w", c.label, entry.PrNumber, err))
			}
			matched = true
			break
		}
		if matched {
			if entry.ActiveBatchID.Valid && deps.Batch != nil {
				if err := deps.Batch.OnMemberRemoved(ctx, entry.TargetBranch, entry.ActiveBatchID.Int64, entry.ID); err != nil {
					result.Errors = append(result.Errors, fmt.Errorf("batch member removed PR #%d: %w", entry.PrNumber, err))
				}
			}
			continue
		}
		handleSuccessTimeout(ctx, deps, result, &entry)
		handleTestingTimeout(ctx, deps, result, &entry)
	}
}

// handleSuccessTimeout removes entries that reported success but were never
// merged by the forge within SuccessTimeout, which usually points at a branch
// protection misconfiguration.
func handleSuccessTimeout(ctx context.Context, deps *Deps, result *PollResult, entry *pg.QueueEntry) {
	if entry.State != pg.EntryStateSuccess || deps.SuccessTimeout <= 0 ||
		!entry.CompletedAt.Valid || time.Since(entry.CompletedAt.Time) <= deps.SuccessTimeout {
		return
	}

	targetURL := forge.DashboardPRURL(deps.ExternalURL, deps.Forge.Kind(), deps.Owner, deps.Repo, entry.PrNumber)
	_ = deps.Forge.SetMQStatus(ctx, deps.Owner, deps.Repo, entry.PrHeadSha, forge.MQStatus{
		State: pg.CheckStateError, Description: "Automerge did not complete in time", TargetURL: targetURL,
	})
	_ = deps.Queue.SetError(ctx, deps.RepoID, entry.PrNumber, "automerge did not complete in time")

	if err := removePR(ctx, deps, result, entry, removeOpts{
		cancelAutomerge: true,
		comment:         "⚠️ Removed from merge queue: PR was marked as ready to merge but Gitea did not merge it in time. This may indicate a branch protection issue.",
		advance:         true,
		logMsg:          "removed PR due to success-but-not-merged timeout",
	}); err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("dequeue timed-out PR #%d: %w", entry.PrNumber, err))
	}
}

// handleTestingTimeout removes entries that have been in "testing" longer
// than CheckTimeout without reaching a terminal state. The webhook-driven
// timeout in monitor only fires when a check status arrives; if the CI never
// reports (e.g. it lost the build after a restart), the queue stalls.
func handleTestingTimeout(ctx context.Context, deps *Deps, result *PollResult, entry *pg.QueueEntry) {
	// Batch members time out via the batch's own clock (per-rebuild), not the
	// per-entry clock which is never reset across bisection.
	if entry.ActiveBatchID.Valid {
		return
	}
	if entry.State != pg.EntryStateTesting || deps.CheckTimeout <= 0 ||
		!entry.TestingStartedAt.Valid || time.Since(entry.TestingStartedAt.Time) <= deps.CheckTimeout {
		return
	}

	targetURL := forge.DashboardPRURL(deps.ExternalURL, deps.Forge.Kind(), deps.Owner, deps.Repo, entry.PrNumber)
	_ = deps.Forge.SetMQStatus(ctx, deps.Owner, deps.Repo, entry.PrHeadSha, forge.MQStatus{
		State: pg.CheckStateError, Description: "CI did not report within timeout", TargetURL: targetURL,
	})
	_ = deps.Queue.SetError(ctx, deps.RepoID, entry.PrNumber, "CI did not report within timeout")
	merge.CleanupMergeBranch(ctx, deps.Forge, deps.Owner, deps.Repo, entry)

	if err := removePR(ctx, deps, result, entry, removeOpts{
		cancelAutomerge: true,
		comment:         "⚠️ Removed from merge queue: CI did not report a status within the timeout. The CI server may have lost the build.",
		advance:         true,
		logMsg:          "removed PR due to testing timeout",
	}); err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("dequeue timed-out testing PR #%d: %w", entry.PrNumber, err))
	}
}

// startQueuedHeads kicks off testing for any target branch whose head entry is
// still in the queued state.
func startQueuedHeads(ctx context.Context, deps *Deps, result *PollResult) {
	activeEntries, err := deps.Queue.ListActiveEntries(ctx, deps.RepoID)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("list active entries for testing: %w", err))
		return
	}

	seenBranches := make(map[string]bool)
	for _, entry := range activeEntries {
		if seenBranches[entry.TargetBranch] {
			continue
		}
		seenBranches[entry.TargetBranch] = true

		head, err := deps.Queue.Head(ctx, deps.RepoID, entry.TargetBranch)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("get head for branch %s: %w", entry.TargetBranch, err))
			continue
		}
		if head == nil || head.State != pg.EntryStateQueued {
			continue
		}

		if deps.SkipQueueIfUpToDate && tryFastForwardSuccess(ctx, deps, result, head) {
			continue
		}

		if deps.Batch.Enabled() {
			if _, err := deps.Batch.FormAndBuild(ctx, entry.TargetBranch); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("form batch for %s: %w", entry.TargetBranch, err))
			}
			continue
		}

		startResult, err := merge.StartTesting(ctx, deps.Forge, deps.Queue, deps.Owner, deps.Repo, deps.RepoID, head, deps.ExternalURL)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("start testing for PR #%d: %w", head.PrNumber, err))
			continue
		}
		if startResult.Removed {
			result.Dequeued = append(result.Dequeued, head.PrNumber)
			result.Errors = append(result.Errors, fmt.Errorf("removed PR #%d from queue during testing start", head.PrNumber))
			slog.Info("head-of-queue was removed, will retry next cycle", "pr", head.PrNumber)
		} else {
			slog.Info("started testing for head-of-queue", "pr", head.PrNumber, "branch", startResult.MergeBranchName)
		}
	}
}

// tryFastForwardSuccess promotes head straight to success when it already
// contains the target branch tip. Returns true when the entry was handled.
// Compare errors fall through to StartTesting so a flaky API never stalls
// the queue.
func tryFastForwardSuccess(ctx context.Context, deps *Deps, result *PollResult, head *pg.QueueEntry) bool {
	upToDate, err := deps.Forge.IsUpToDate(ctx, deps.Owner, deps.Repo, head.TargetBranch, head.PrHeadSha)
	if err != nil {
		slog.Warn("up-to-date check failed, falling back to merge branch", "pr", head.PrNumber, "error", err)
		return false
	}
	if !upToDate {
		return false
	}

	targetURL := forge.DashboardPRURL(deps.ExternalURL, deps.Forge.Kind(), deps.Owner, deps.Repo, head.PrNumber)
	_ = deps.Forge.SetMQStatus(ctx, deps.Owner, deps.Repo, head.PrHeadSha, forge.MQStatus{
		State: pg.CheckStateSuccess, Description: "Already up to date with target branch", TargetURL: targetURL,
	})
	if err := deps.Queue.UpdateState(ctx, deps.RepoID, head.PrNumber, pg.EntryStateSuccess); err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("update state to success for PR #%d: %w", head.PrNumber, err))
		return true
	}
	slog.Info("skipped merge-branch testing: PR already up to date with target", "pr", head.PrNumber)
	return true
}

// Run starts the polling loop. The first poll happens immediately.
func Run(ctx context.Context, deps *Deps, interval time.Duration) {
	slog.Info("poller started", "owner", deps.Owner, "repo", deps.Repo, "interval", interval)

	// periodic ticks log paused/issue diagnostics; the initial and
	// webhook-triggered polls stay quiet to avoid log noise on bursts.
	doPoll := func(periodic bool) {
		result, err := PollOnce(ctx, deps)
		if err != nil {
			slog.Error("poll error", "owner", deps.Owner, "repo", deps.Repo, "error", err)
			return
		}
		if !periodic {
			return
		}
		if result.Paused {
			slog.Warn("forge unavailable, pausing", "owner", deps.Owner, "repo", deps.Repo)
		}
		for _, e := range result.Errors {
			slog.Warn("poll issue", "owner", deps.Owner, "repo", deps.Repo, "error", e)
		}
	}

	doPoll(false)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("poller stopped", "owner", deps.Owner, "repo", deps.Repo)
			return
		case <-deps.Trigger:
			doPoll(false)
		case <-ticker.C:
			doPoll(true)
		}
	}
}
