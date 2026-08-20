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
	"github.com/Mic92/gitea-mq/internal/logutil"
	"github.com/Mic92/gitea-mq/internal/merge"
	"github.com/Mic92/gitea-mq/internal/monitor"
	"github.com/Mic92/gitea-mq/internal/queue"
	"github.com/Mic92/gitea-mq/internal/store/pg"
	"github.com/jackc/pgx/v5/pgtype"
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
	// MergeLabel enqueues labeled PRs (stack-aware on GitHub); empty disables.
	MergeLabel string
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
	// IdleGating lets the periodic reconcile skip repos with no live queue
	// work. Safe only on forges that deliver CI status via webhooks (GitHub,
	// Gitea >= 1.24); must stay false for forges that need status polling.
	IdleGating bool
	// Now overrides the wall clock in timeout checks; nil means time.Now.
	// Tests use it instead of sleeping past real timeouts.
	Now func() time.Time
	// Ticks replaces Run's periodic ticker when non-nil so tests can drive
	// polls deterministically.
	Ticks <-chan time.Time
	// TickDone, when non-nil, is signalled after each trigger/tick has been
	// fully handled, letting tests sequence polls without sleeping.
	TickDone chan<- struct{}

	// hintedSHAs dedupes stack-hint statuses per head SHA.
	hintedSHAs map[string]bool
}

func (d *Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

type PollResult struct {
	Enqueued []int64
	Dequeued []int64
	Advanced []int64
	Errors   []error
	Paused   bool // forge unreachable
}

type removeOpts struct {
	cancelIntent bool
	comment      string
	advance      bool
	logMsg       string
	logAttrs     []any
}

func removePR(ctx context.Context, deps *Deps, result *PollResult, entry *pg.QueueEntry, opts removeOpts) error {
	dqResult, err := deps.Queue.Dequeue(ctx, deps.RepoID, entry.PrNumber)
	if err != nil {
		return err
	}

	if opts.cancelIntent {
		logutil.WarnIfErr(forge.CancelMergeIntent(ctx, deps.Forge, deps.Owner, deps.Repo, entry.PrNumber, deps.MergeLabel), "cancel merge intent failed", "pr", entry.PrNumber)
	}
	if opts.comment != "" {
		logutil.WarnIfErr(deps.Forge.Comment(ctx, deps.Owner, deps.Repo, entry.PrNumber, opts.comment), "post comment failed", "pr", entry.PrNumber)
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

// prCheckResult evaluates the PR's own head-commit checks against the
// required checks. Reports CheckSuccess when no CI is configured at all.
func prCheckResult(ctx context.Context, deps *Deps, pr *forge.PR, targetBranch string) (monitor.CheckResult, error) {
	requiredChecks, err := monitor.ResolveRequiredChecks(ctx, deps.Forge, deps.Owner, deps.Repo, targetBranch, deps.FallbackChecks)
	if err != nil {
		return monitor.CheckWaiting, fmt.Errorf("resolve required checks for PR #%d: %w", pr.Number, err)
	}

	checks, err := deps.Forge.GetCheckStates(ctx, deps.Owner, deps.Repo, pr.HeadSHA)
	if err != nil {
		return monitor.CheckWaiting, fmt.Errorf("get check states for PR #%d: %w", pr.Number, err)
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
		return monitor.CheckSuccess, nil
	}

	res, _, _ := monitor.EvaluateChecks(externalStatuses, requiredChecks)
	return res, nil
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
	enqueueLabeledPRs(ctx, deps, result, openPRs)
	hintStackedPRs(ctx, deps, openPRs)
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
		MergeLabel:     deps.MergeLabel,
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

		enqueuePR(ctx, deps, result, pr, pr.BaseBranch, "automerge detection")
	}
}

// enqueuePR adds a PR to the queue for targetBranch once its own CI is green.
func enqueuePR(ctx context.Context, deps *Deps, result *PollResult, pr *forge.PR, targetBranch, source string) {
	res, err := prCheckResult(ctx, deps, pr, targetBranch)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("check CI status for PR #%d: %w", pr.Number, err))
		return
	}
	if res != monitor.CheckSuccess {
		return
	}

	enqResult, err := deps.Queue.Enqueue(ctx, deps.RepoID, pr.Number, pr.HeadSHA, targetBranch)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("enqueue PR #%d: %w", pr.Number, err))
		return
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
		slog.Info("enqueued PR from "+source, "pr", pr.Number, "position", enqResult.Position)
	}
}

// enqueueLabeledPRs adds open PRs carrying the merge label. For stacked PRs
// the queue target is the stack's base branch, not the parent PR's branch.
func enqueueLabeledPRs(ctx context.Context, deps *Deps, result *PollResult, openPRs []forge.PR) {
	if deps.MergeLabel == "" {
		return
	}
	for i := range openPRs {
		pr := &openPRs[i]
		if !pr.HasLabel(deps.MergeLabel) {
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

		target, err := labeledTargetBranch(ctx, deps, pr)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("resolve stack for PR #%d: %w", pr.Number, err))
			continue
		}
		enqueuePR(ctx, deps, result, pr, target, "merge label")
	}
}

// labeledTargetBranch is the stack base branch for stacked PRs, else the PR's
// own base branch.
func labeledTargetBranch(ctx context.Context, deps *Deps, pr *forge.PR) (string, error) {
	stack, err := forge.ResolveStack(ctx, deps.Forge, deps.Owner, deps.Repo, pr.Number)
	if err != nil {
		return "", err
	}
	if stack != nil {
		if _, ok := stack.MembersUpTo(pr.Number); ok {
			return stack.BaseBranch, nil
		}
	}
	return pr.BaseBranch, nil
}

// hintStackedPRs surfaces the label workflow on stacked PRs, whose required
// gitea-mq check would otherwise sit unset with no explanation: automerge is
// not offered for stacks, so nothing would ever trigger the queue.
func hintStackedPRs(ctx context.Context, deps *Deps, openPRs []forge.PR) {
	if deps.MergeLabel == "" {
		return
	}
	if _, ok := deps.Forge.(forge.StackResolver); !ok {
		return
	}

	byHeadBranch := make(map[string]*forge.PR, len(openPRs))
	for i := range openPRs {
		byHeadBranch[openPRs[i].HeadBranch] = &openPRs[i]
	}
	stacked := make(map[int64]*forge.PR)
	for i := range openPRs {
		pr := &openPRs[i]
		if parent, ok := byHeadBranch[pr.BaseBranch]; ok {
			stacked[pr.Number] = pr
			stacked[parent.Number] = parent
		}
	}

	for _, pr := range stacked {
		if pr.HasLabel(deps.MergeLabel) || deps.hintedSHAs[pr.HeadSHA] {
			continue
		}
		entry, err := deps.Queue.GetEntry(ctx, deps.RepoID, pr.Number)
		if err != nil || entry != nil {
			continue
		}
		targetURL := forge.DashboardPRURL(deps.ExternalURL, deps.Forge.Kind(), deps.Owner, deps.Repo, pr.Number)
		if err := deps.Forge.SetMQStatus(ctx, deps.Owner, deps.Repo, pr.HeadSHA, forge.MQStatus{
			State:       pg.CheckStatePending,
			Description: fmt.Sprintf("Stack detected — add the '%s' label to the topmost PR you want to merge", deps.MergeLabel),
			TargetURL:   targetURL,
		}); err != nil {
			slog.Warn("set stack hint status failed", "pr", pr.Number, "error", err)
			continue
		}
		if deps.hintedSHAs == nil {
			deps.hintedSHAs = make(map[string]bool)
		}
		deps.hintedSHAs[pr.HeadSHA] = true
	}
}

// finalizeLabeledMerge completes label-triggered entries that passed the
// queue: the forge has no automerge armed for them, so gitea-mq flips the
// remaining stack heads green and performs the (stack-aware) merge itself.
func finalizeLabeledMerge(ctx context.Context, deps *Deps, result *PollResult, entry *pg.QueueEntry, pr *forge.PR) {
	if deps.MergeLabel == "" || entry.State != pg.EntryStateSuccess || pr == nil || pr.Merged || !pr.HasLabel(deps.MergeLabel) {
		return
	}

	stack, err := forge.ResolveStack(ctx, deps.Forge, deps.Owner, deps.Repo, entry.PrNumber)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("resolve stack for PR #%d: %w", entry.PrNumber, err))
		return
	}
	if stack != nil {
		members, _ := stack.MembersUpTo(entry.PrNumber)
		targetURL := forge.DashboardPRURL(deps.ExternalURL, deps.Forge.Kind(), deps.Owner, deps.Repo, entry.PrNumber)
		for _, m := range members {
			if m.Merged || m.HeadSHA == entry.PrHeadSha {
				continue
			}
			logutil.WarnIfErr(deps.Forge.SetMQStatus(ctx, deps.Owner, deps.Repo, m.HeadSHA, forge.MQStatus{
				State: pg.CheckStateSuccess, Description: "Merge queue passed (stack)", TargetURL: targetURL,
			}), "set stack member status failed", "pr", m.Number)
		}
	}

	if err := deps.Forge.MergePR(ctx, deps.Owner, deps.Repo, entry.PrNumber); err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("merge labeled PR #%d: %w", entry.PrNumber, err))
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

		labeled := deps.MergeLabel != "" && pr.HasLabel(deps.MergeLabel)
		retargeted := pr.BaseBranch != "" && pr.BaseBranch != entry.TargetBranch
		if retargeted && labeled {
			if target, err := labeledTargetBranch(ctx, deps, pr); err == nil && target == entry.TargetBranch {
				retargeted = false
			}
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
				when:  retargeted,
				label: "retargeted",
				opts: removeOpts{
					cancelIntent: true,
					comment:      fmt.Sprintf("⚠️ Removed from merge queue: target branch changed from `%s` to `%s`. Please re-schedule the merge.", entry.TargetBranch, pr.BaseBranch),
					logMsg:       "removed retargeted PR from queue",
					logAttrs:     []any{"old_branch", entry.TargetBranch, "new_branch", pr.BaseBranch},
				},
			},
			{
				when:  pr.HeadSHA != "" && pr.HeadSHA != entry.PrHeadSha,
				label: "pushed",
				opts: removeOpts{
					cancelIntent: true,
					comment:      "⚠️ Removed from merge queue: new commits were pushed. Please re-schedule the merge.",
					advance:      true,
					logMsg:       "removed PR due to new push",
				},
			},
			{
				when:  !pr.AutoMergeEnabled && !labeled,
				label: "cancelled",
				opts:  removeOpts{logMsg: "removed PR due to merge intent cancellation"},
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
		finalizeLabeledMerge(ctx, deps, result, &entry, pr)
		handleSuccessTimeout(ctx, deps, result, &entry, pr)
		handleTestingTimeout(ctx, deps, result, &entry)
	}
}

// handleSuccessTimeout removes entries that reported success but were never
// merged by the forge within SuccessTimeout, which usually points at a branch
// protection misconfiguration. While the PR's own required checks are still
// pending the forge legitimately cannot merge yet, so removal is deferred —
// but only up to CheckTimeout, so a stuck CI cannot block the queue forever.
func handleSuccessTimeout(ctx context.Context, deps *Deps, result *PollResult, entry *pg.QueueEntry, pr *forge.PR) {
	if entry.State != pg.EntryStateSuccess || !timedOut(deps.now(), entry.CompletedAt, deps.SuccessTimeout) {
		return
	}

	if pr != nil && !timedOut(deps.now(), entry.CompletedAt, deps.CheckTimeout) {
		res, err := prCheckResult(ctx, deps, pr, pr.BaseBranch)
		if err != nil {
			slog.Warn("success timeout: could not evaluate PR checks", "pr", entry.PrNumber, "error", err)
		} else if res == monitor.CheckWaiting {
			slog.Info("success timeout deferred: PR checks still pending", "pr", entry.PrNumber)
			return
		}
	}

	removeTimedOut(ctx, deps, result, entry, timedOutRemoval{
		statusDescription: "Merge did not complete in time",
		errorMessage:      "merge did not complete in time",
		comment:           "⚠️ Removed from merge queue: PR was marked as ready to merge but the forge did not merge it in time. This may indicate a branch protection issue.",
		logMsg:            "removed PR due to success-but-not-merged timeout",
	})
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
	if entry.State != pg.EntryStateTesting || !timedOut(deps.now(), entry.TestingStartedAt, deps.CheckTimeout) {
		return
	}

	merge.CleanupMergeBranch(ctx, deps.Forge, deps.Owner, deps.Repo, entry)
	removeTimedOut(ctx, deps, result, entry, timedOutRemoval{
		statusDescription: "CI did not report within timeout",
		errorMessage:      "CI did not report within timeout",
		comment:           "⚠️ Removed from merge queue: CI did not report a status within the timeout. The CI server may have lost the build.",
		logMsg:            "removed PR due to testing timeout",
	})
}

// timedOut reports whether ts is set and lies more than timeout before now.
// A non-positive timeout disables the check.
func timedOut(now time.Time, ts pgtype.Timestamptz, timeout time.Duration) bool {
	return timeout > 0 && ts.Valid && now.Sub(ts.Time) > timeout
}

// timedOutRemoval describes how a timed-out entry is reported before removal.
type timedOutRemoval struct {
	statusDescription string // MQ commit status shown on the PR head
	errorMessage      string // stored on the queue entry
	comment           string // posted on the PR
	logMsg            string
}

// removeTimedOut marks the entry's MQ status and queue error, then dequeues it
// (cancelling automerge and advancing the queue).
func removeTimedOut(ctx context.Context, deps *Deps, result *PollResult, entry *pg.QueueEntry, opts timedOutRemoval) {
	targetURL := forge.DashboardPRURL(deps.ExternalURL, deps.Forge.Kind(), deps.Owner, deps.Repo, entry.PrNumber)
	logutil.WarnIfErr(deps.Forge.SetMQStatus(ctx, deps.Owner, deps.Repo, entry.PrHeadSha, forge.MQStatus{
		State: pg.CheckStateError, Description: opts.statusDescription, TargetURL: targetURL,
	}), "set mq status failed", "pr", entry.PrNumber)
	logutil.WarnIfErr(deps.Queue.SetError(ctx, deps.RepoID, entry.PrNumber, opts.errorMessage), "set queue error failed", "pr", entry.PrNumber)

	if err := removePR(ctx, deps, result, entry, removeOpts{
		cancelIntent: true,
		comment:      opts.comment,
		advance:      true,
		logMsg:       opts.logMsg,
	}); err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("dequeue timed-out PR #%d: %w", entry.PrNumber, err))
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

		startResult, err := merge.StartTesting(ctx, deps.Forge, deps.Queue, deps.Owner, deps.Repo, deps.RepoID, head, deps.ExternalURL, deps.MergeLabel)
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
	logutil.WarnIfErr(deps.Forge.SetMQStatus(ctx, deps.Owner, deps.Repo, head.PrHeadSha, forge.MQStatus{
		State: pg.CheckStateSuccess, Description: "Already up to date with target branch", TargetURL: targetURL,
	}), "set mq status failed", "pr", head.PrNumber)
	if err := deps.Queue.UpdateState(ctx, deps.RepoID, head.PrNumber, pg.EntryStateSuccess); err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("update state to success for PR #%d: %w", head.PrNumber, err))
		return true
	}
	slog.Info("skipped merge-branch testing: PR already up to date with target", "pr", head.PrNumber)
	return true
}

// hasActiveWork reports whether the repo has live queue state. It only reads
// the local DB, so idle repos can be skipped without spending forge API quota.
func hasActiveWork(ctx context.Context, deps *Deps) bool {
	entries, err := deps.Queue.ListActiveEntries(ctx, deps.RepoID)
	if err != nil {
		// Fail open: keep reconciling rather than stall on a transient DB error.
		slog.Warn("active-work check failed, polling anyway", "owner", deps.Owner, "repo", deps.Repo, "error", err)
		return true
	}
	return len(entries) > 0
}

func notifyTickDone(deps *Deps) {
	if deps.TickDone != nil {
		deps.TickDone <- struct{}{}
	}
}

// Run starts the polling loop. The first poll happens immediately. idleInterval
// throttles reconciles for idle repos only when deps.IdleGating is set.
func Run(ctx context.Context, deps *Deps, interval, idleInterval time.Duration) {
	slog.Info("poller started", "owner", deps.Owner, "repo", deps.Repo, "interval", interval, "idle_interval", idleInterval, "idle_gating", deps.IdleGating)

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

	// Idle repos reconcile at idleInterval instead of every tick, keeping
	// periodic forge traffic proportional to repos with live queue work.
	if idleInterval < interval {
		idleInterval = interval
	}
	ticks := deps.Ticks
	if ticks == nil {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		ticks = ticker.C
	}
	lastFull := time.Now()

	for {
		select {
		case <-ctx.Done():
			slog.Info("poller stopped", "owner", deps.Owner, "repo", deps.Repo)
			return
		case <-deps.Trigger:
			doPoll(false)
			lastFull = time.Now()
			notifyTickDone(deps)
		case <-ticks:
			skipIdle := deps.IdleGating && !hasActiveWork(ctx, deps) && time.Since(lastFull) < idleInterval
			if !skipIdle {
				doPoll(true)
				lastFull = time.Now()
			}
			notifyTickDone(deps)
		}
	}
}
