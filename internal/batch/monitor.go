package batch

import (
	"context"

	"github.com/Mic92/gitea-mq/internal/monitor"
	"github.com/Mic92/gitea-mq/internal/store/pg"
)

// HandleCheck implements monitor.BatchHandler. It is the single entry point
// from the check pipeline into the batch engine: lock, guard against stale
// SHAs, and only then persist the check and evaluate. Persisting after the
// guard is what stops a late event for a superseded build from polluting the
// ledger that decides the current one.
func (e *Engine) HandleCheck(ctx context.Context, entry *pg.QueueEntry, checkCtx string, state pg.CheckState, targetURL string) error {
	defer e.lock(entry.TargetBranch)()

	b, err := e.Queue.GetBatch(ctx, entry.ActiveBatchID.Int64)
	if err != nil || b == nil || b.State != pg.BatchStateTesting {
		return err
	}
	if !entry.MergeBranchSha.Valid || entry.MergeBranchSha.String != b.BranchSha.String {
		return nil
	}

	if err := e.Queue.SaveCheckStatus(ctx, entry.ID, checkCtx, state, targetURL); err != nil {
		return err
	}
	required, err := monitor.ResolveRequiredChecks(ctx, e.Forge, e.Owner, e.Repo, b.TargetBranch, e.FallbackChecks)
	if err != nil {
		return err
	}
	statuses, err := e.Queue.GetCheckStatuses(ctx, entry.ID)
	if err != nil {
		return err
	}

	switch r, fc, fu := monitor.EvaluateChecks(statuses, required); r {
	case monitor.CheckSuccess:
		return e.HandlePass(ctx, b)
	case monitor.CheckFailure:
		return e.HandleFail(ctx, b, fc, fu)
	default:
		if TimedOut(b, e.CheckTimeout) {
			return e.HandleFail(ctx, b, "timeout", "")
		}
	}
	return nil
}

