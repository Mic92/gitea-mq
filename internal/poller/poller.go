// Package poller reconciles the queue with the forge: it discovers PRs with
// auto-merge enabled, enqueues them once their own CI is green, and removes
// queued PRs that were merged/closed/retargeted/pushed/cancelled.
package poller

import (
	"context"
	"fmt"
	"log/slog"
	"time"

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
	if dqResult.WasHead {
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

	// Enqueue newly auto-merge-enabled PRs whose own CI is green.
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

	// Reconcile existing queue entries against forge state.
	activeEntries, err := deps.Queue.ListActiveEntries(ctx, deps.RepoID)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("list active entries: %w", err))
		return result, nil
	}

	for _, entry := range activeEntries {
		pr, isOpen := openPRMap[entry.PrNumber]

		if !isOpen {
			fullPR, err := deps.Forge.GetPR(ctx, deps.Owner, deps.Repo, entry.PrNumber)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("get PR #%d: %w", entry.PrNumber, err))
				continue
			}

			if fullPR.Merged {
				if err := removePR(ctx, deps, result, &entry, removeOpts{
					advance: true, logMsg: "removed merged PR from queue",
				}); err != nil {
					result.Errors = append(result.Errors, fmt.Errorf("dequeue merged PR #%d: %w", entry.PrNumber, err))
				}
				continue
			}

			if err := removePR(ctx, deps, result, &entry, removeOpts{
				logMsg: "removed closed PR from queue",
			}); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("dequeue closed PR #%d: %w", entry.PrNumber, err))
			}
			continue
		}

		if pr.BaseBranch != "" && pr.BaseBranch != entry.TargetBranch {
			if err := removePR(ctx, deps, result, &entry, removeOpts{
				cancelAutomerge: true,
				comment:         fmt.Sprintf("⚠️ Removed from merge queue: target branch changed from `%s` to `%s`. Please re-schedule automerge.", entry.TargetBranch, pr.BaseBranch),
				logMsg:          "removed retargeted PR from queue",
				logAttrs:        []any{"old_branch", entry.TargetBranch, "new_branch", pr.BaseBranch},
			}); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("dequeue retargeted PR #%d: %w", entry.PrNumber, err))
			}
			continue
		}

		if pr.HeadSHA != "" && pr.HeadSHA != entry.PrHeadSha {
			if err := removePR(ctx, deps, result, &entry, removeOpts{
				cancelAutomerge: true,
				comment:         "⚠️ Removed from merge queue: new commits were pushed. Please re-schedule automerge.",
				advance:         true,
				logMsg:          "removed PR due to new push",
			}); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("dequeue pushed PR #%d: %w", entry.PrNumber, err))
			}
			continue
		}

		if !pr.AutoMergeEnabled {
			if err := removePR(ctx, deps, result, &entry, removeOpts{
				logMsg: "removed PR due to automerge cancellation",
			}); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("dequeue cancelled PR #%d: %w", entry.PrNumber, err))
			}
			continue
		}

		if entry.State == pg.EntryStateSuccess && deps.SuccessTimeout > 0 &&
			entry.CompletedAt.Valid && time.Since(entry.CompletedAt.Time) > deps.SuccessTimeout {
			targetURL := forge.DashboardPRURL(deps.ExternalURL, deps.Forge.Kind(), deps.Owner, deps.Repo, entry.PrNumber)
			_ = deps.Forge.SetMQStatus(ctx, deps.Owner, deps.Repo, entry.PrHeadSha, forge.MQStatus{
				State: pg.CheckStateError, Description: "Automerge did not complete in time", TargetURL: targetURL,
			})
			_ = deps.Queue.SetError(ctx, deps.RepoID, entry.PrNumber, "automerge did not complete in time")

			if err := removePR(ctx, deps, result, &entry, removeOpts{
				cancelAutomerge: true,
				comment:         "⚠️ Removed from merge queue: PR was marked as ready to merge but Gitea did not merge it in time. This may indicate a branch protection issue.",
				advance:         true,
				logMsg:          "removed PR due to success-but-not-merged timeout",
			}); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("dequeue timed-out PR #%d: %w", entry.PrNumber, err))
			}
		}
	}

	// Kick off testing for any branch whose head is still queued.
	activeEntries, err = deps.Queue.ListActiveEntries(ctx, deps.RepoID)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Errorf("list active entries for testing: %w", err))
		return result, nil
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

	return result, nil
}

// Run starts the polling loop. The first poll happens immediately.
func Run(ctx context.Context, deps *Deps, interval time.Duration) {
	slog.Info("poller started", "owner", deps.Owner, "repo", deps.Repo, "interval", interval)

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
		case <-deps.Trigger:
			if _, err := PollOnce(ctx, deps); err != nil {
				slog.Error("poll error", "owner", deps.Owner, "repo", deps.Repo, "error", err)
			}
		case <-ticker.C:
			result, err := PollOnce(ctx, deps)
			if err != nil {
				slog.Error("poll error", "owner", deps.Owner, "repo", deps.Repo, "error", err)
				continue
			}
			if result.Paused {
				slog.Warn("forge unavailable, pausing", "owner", deps.Owner, "repo", deps.Repo)
			}
			for _, e := range result.Errors {
				slog.Warn("poll issue", "owner", deps.Owner, "repo", deps.Repo, "error", e)
			}
		}
	}
}
