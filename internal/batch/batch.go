// Package batch implements bors-style batch testing: build a single merge
// branch from N queued PRs, fast-forward the target on green, and bisect on
// red. All state lives on one pg.Batch row so the loop is restart-safe.
package batch

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/Mic92/gitea-mq/internal/forge"
	"github.com/Mic92/gitea-mq/internal/merge"
	"github.com/Mic92/gitea-mq/internal/queue"
	"github.com/Mic92/gitea-mq/internal/store/pg"
	"github.com/jackc/pgx/v5/pgtype"
)

// MaxFFRetries caps consecutive ErrNotFastForward rebuilds before the
// remaining current members are ejected with an actionable error.
const MaxFFRetries = 3

// BranchPrefix is the namespace for batch merge branches.
const BranchPrefix = merge.BranchPrefix + "batch/"

// BranchName returns the canonical batch branch name.
func BranchName(id int64) string {
	return fmt.Sprintf("%s%d", BranchPrefix, id)
}

// Engine drives the batch lifecycle for one repository. The pg.Batch row is
// the source of truth; the mutex serialises every public entry point so the
// webhook handler and poller cannot mutate the same batch concurrently.
type Engine struct {
	Forge       forge.Forge
	Queue       *queue.Service
	Owner       string
	Repo        string
	RepoID      int64
	ExternalURL string

	BatchMax       int
	BisectMaxSteps int
	CheckTimeout   time.Duration
	FallbackChecks []string

	// MergedPoll controls ensureMergedOrClose. Defaults: 200ms × 50 = 10s.
	MergedPollInterval time.Duration
	MergedPollAttempts int

	// Advance is called whenever a batch finishes so the next root can be
	// formed without waiting for the poll tick. Optional.
	Advance func()

	mu    sync.Mutex // guards locks
	locks map[string]*sync.Mutex
}

// lock returns the per-target-branch unlock func. Batches for different
// branches in the same repo are independent (ux_batches_live is per branch),
// so serialising them on one mutex would head-of-line block on forge I/O.
func (e *Engine) lock(targetBranch string) func() {
	e.mu.Lock()
	if e.locks == nil {
		e.locks = make(map[string]*sync.Mutex)
	}
	l := e.locks[targetBranch]
	if l == nil {
		l = &sync.Mutex{}
		e.locks[targetBranch] = l
	}
	e.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// Enabled reports whether batching is active. BatchMax==1 keeps the legacy
// single-PR path byte-for-byte intact.
func (e *Engine) Enabled() bool { return e != nil && e.BatchMax != 1 }

// pendingStack is the JSONB stack of int64 slices stored on the batch row.
type pendingStack [][]int64

func loadPending(raw []byte) pendingStack {
	var p pendingStack
	if len(raw) == 0 {
		return p
	}
	_ = json.Unmarshal(raw, &p)
	return p
}

func (p pendingStack) bytes() []byte {
	if len(p) == 0 {
		return []byte("[]")
	}
	b, _ := json.Marshal(p)
	return b
}

func (p pendingStack) drop(id int64) pendingStack {
	eq := func(v int64) bool { return v == id }
	out := p[:0]
	for _, s := range p {
		if s = slices.DeleteFunc(s, eq); len(s) > 0 {
			out = append(out, s)
		}
	}
	return out
}

// FormAndBuild forms a new root batch from the queued head and builds its
// branch. Returns nil when there is nothing to do (queue empty or a live
// batch already exists). Safe to call on every poll tick.
func (e *Engine) FormAndBuild(ctx context.Context, targetBranch string) (*pg.Batch, error) {
	defer e.lock(targetBranch)()
	live, err := e.Queue.GetLiveBatch(ctx, e.RepoID, targetBranch)
	if err != nil {
		return nil, err
	}
	if live != nil {
		// A forming batch means a previous Build errored mid-run; retry it
		// so a transient forge failure does not stall the queue until restart.
		if live.State == pg.BatchStateForming {
			return live, e.Build(ctx, live)
		}
		return nil, nil
	}
	b, err := e.Queue.FormBatch(ctx, e.RepoID, targetBranch, e.BatchMax)
	if err != nil || b == nil {
		return nil, err
	}
	slog.Info("batch formed", "batch", b.ID, "branch", targetBranch, "members", len(b.MemberIds))
	return b, e.Build(ctx, b)
}

// Build constructs the batch branch by merging current_ids onto the target
// tip. Conflicting members are ejected and construction continues. On success
// the batch transitions to testing and every current entry's merge_branch_*
// is set to the batch branch so the existing SHA-routing keeps working.
func (e *Engine) Build(ctx context.Context, b *pg.Batch) error {
	if !b.BranchName.Valid {
		b.BranchName = pgtype.Text{String: BranchName(b.ID), Valid: true}
	}
	branch := b.BranchName.String
	_ = e.Forge.DeleteBranch(ctx, e.Owner, e.Repo, branch)

	entries, err := e.Queue.GetEntriesByIDs(ctx, b.CurrentIds)
	if err != nil {
		return fmt.Errorf("load batch members: %w", err)
	}

	heads := make([]string, len(entries))
	for i := range entries {
		heads[i] = entries[i].PrHeadSha
	}
	tip, steps, err := e.stack(ctx, b.TargetBranch, heads, branch)
	if err != nil {
		// Whole-operation failure (clone/push). Row stays forming;
		// FormAndBuild retries on the next tick.
		return fmt.Errorf("build batch branch: %w", err)
	}

	var (
		surv []pg.QueueEntry
		prev int64 // previous PR number for conflict attribution
	)
	for i := range entries {
		ent, step := &entries[i], steps[i]
		switch {
		case step.Conflict:
			msg := "Merge conflict with target branch"
			if prev != 0 {
				msg = fmt.Sprintf("Merge conflict in batch (after #%d)", prev)
			}
			e.eject(ctx, b, ent, pg.CheckStateFailure, msg,
				"❌ Removed from merge queue: "+msg+". Please rebase and re-schedule automerge.")
		case step.Err != nil:
			e.eject(ctx, b, ent, pg.CheckStateError, "Failed to create merge branch",
				fmt.Sprintf("❌ Removed from merge queue: failed to create merge branch.\n\n```\n%v\n```", step.Err))
		default:
			surv = append(surv, *ent)
			prev = ent.PrNumber
		}
	}

	b.CurrentIds = ids(surv)
	if len(surv) == 0 {
		return e.next(ctx, b)
	}

	b.BranchSha = pgtype.Text{String: tip, Valid: true}
	b.State = pg.BatchStateTesting
	b.TestingStartedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	first := b.Builds == 0
	b.Builds++
	if err := e.Queue.SaveBatch(ctx, b); err != nil {
		return err
	}

	// Route check events: every current entry carries the batch branch/sha.
	// Clearing the check ledger comes last so a stale SaveCheckStatus that
	// raced in before SetMergeBranch (still keyed on the old SHA) is wiped.
	desc := fmt.Sprintf("Testing batch #%d (%d PRs)", b.ID, len(b.MemberIds))
	for _, ent := range surv {
		_ = e.Queue.SetMergeBranch(ctx, e.RepoID, ent.PrNumber, branch, tip)
		if first {
			_ = e.Forge.SetMQStatus(ctx, e.Owner, e.Repo, ent.PrHeadSha, forge.MQStatus{
				State: pg.CheckStatePending, Description: desc, TargetURL: e.prURL(ent.PrNumber),
			})
		}
	}
	_ = e.Queue.ClearCheckStatuses(ctx, b.CurrentIds)

	slog.Info("batch built", "batch", b.ID, "build", b.Builds, "sha", tip,
		"current", len(surv), "pending", len(loadPending(b.Pending)))
	return nil
}

// HandlePass lands current_ids by fast-forwarding the target branch to the
// tested SHA, then pops the next pending slice.
func (e *Engine) HandlePass(ctx context.Context, b *pg.Batch) error {
	if b.State != pg.BatchStateTesting {
		return nil
	}
	sha := b.BranchSha.String
	if err := e.Forge.FastForward(ctx, e.Owner, e.Repo, b.TargetBranch, sha); err != nil {
		var denied *forge.PushDeniedError
		switch {
		case errors.Is(err, forge.ErrNotFastForward):
			b.FfRetries++
			if b.FfRetries < MaxFFRetries {
				slog.Info("batch fast-forward raced; rebuilding", "batch", b.ID, "retry", b.FfRetries)
				return e.rebuild(ctx, b)
			}
			e.ejectCurrent(ctx, b, pg.CheckStateError, "Target branch moved repeatedly",
				fmt.Sprintf("⚠️ Removed from merge queue: the target branch moved during fast-forward "+
					"%d times in a row — is something else pushing to `%s`?", MaxFFRetries, b.TargetBranch))
		case errors.As(err, &denied):
			e.ejectCurrent(ctx, b, pg.CheckStateError, "gitea-mq cannot push to "+b.TargetBranch,
				fmt.Sprintf("⚠️ Removed from merge queue: gitea-mq is not allowed to push to `%s`.\n\n"+
					"Add the gitea-mq user/app to the branch's push whitelist (or ruleset bypass) and re-schedule.\n\n```\n%s\n```",
					b.TargetBranch, denied.Message))
		default:
			return fmt.Errorf("fast-forward %s: %w", b.TargetBranch, err)
		}
		return e.next(ctx, b)
	}
	b.FfRetries = 0

	entries, err := e.Queue.GetEntriesByIDs(ctx, b.CurrentIds)
	if err != nil {
		return err
	}
	desc := fmt.Sprintf("Merged via batch #%d", b.ID)
	var wg sync.WaitGroup
	for i := range entries {
		ent := &entries[i]
		_ = e.Forge.SetMQStatus(ctx, e.Owner, e.Repo, ent.PrHeadSha, forge.MQStatus{
			State: pg.CheckStateSuccess, Description: desc, TargetURL: e.prURL(ent.PrNumber),
		})
		if _, err := e.Queue.Dequeue(ctx, e.RepoID, ent.PrNumber); err != nil {
			slog.Warn("dequeue landed PR failed", "pr", ent.PrNumber, "err", err)
		}
		wg.Go(func() { e.ensureMergedOrClose(ctx, ent, sha, b.ID) })
	}
	wg.Wait()

	b.LandedIds = append(b.LandedIds, b.CurrentIds...)
	b.CurrentIds = nil
	slog.Info("batch landed", "batch", b.ID, "sha", sha, "prs", len(entries))
	return e.next(ctx, b)
}

// HandleFail bisects: split current, push the second half, rebuild the first.
// A failing singleton is ejected with the failed check attributed.
func (e *Engine) HandleFail(ctx context.Context, b *pg.Batch, failedCheck, targetURL string) error {
	if b.State != pg.BatchStateTesting {
		return nil
	}
	if len(b.CurrentIds) == 1 {
		entries, _ := e.Queue.GetEntriesByIDs(ctx, b.CurrentIds)
		if len(entries) == 1 {
			ref := failedCheck
			if targetURL != "" {
				ref = fmt.Sprintf("[%s](%s)", failedCheck, targetURL)
			}
			e.eject(ctx, b, &entries[0], pg.CheckStateFailure,
				"Check failed: "+failedCheck,
				"❌ Removed from merge queue: Check failed: "+ref)
		}
		b.CurrentIds = nil
		return e.next(ctx, b)
	}

	if e.BisectMaxSteps > 0 && int(b.Builds) >= e.BisectMaxSteps {
		// Cap is on builds, not splits: drain pending too so next() finishes
		// instead of popping another slice and rebuilding past the cap.
		for _, s := range loadPending(b.Pending) {
			b.CurrentIds = append(b.CurrentIds, s...)
		}
		b.Pending = nil
		e.ejectCurrent(ctx, b, pg.CheckStateError,
			"Bisection limit reached",
			fmt.Sprintf("⚠️ Removed from merge queue: batch bisection reached the configured limit of %d builds.", e.BisectMaxSteps))
		return e.next(ctx, b)
	}

	mid := len(b.CurrentIds) / 2
	left := slices.Clone(b.CurrentIds[:mid])
	right := slices.Clone(b.CurrentIds[mid:])
	pending := append(loadPending(b.Pending), right)
	b.Pending = pending.bytes()
	b.CurrentIds = left
	// Right-half members leave the branch; clear their routing so late events
	// for this build's SHA cannot reach them.
	_ = e.Queue.ClearMergeBranch(ctx, right)
	slog.Info("batch bisecting", "batch", b.ID, "build", b.Builds,
		"failed_check", failedCheck, "left", len(left), "right", len(right))
	return e.rebuild(ctx, b)
}

// HandleTimeout treats a CI timeout as a batch failure. The batch is reloaded
// under the lock so a poller snapshot that raced a webhook-driven rebuild
// cannot bisect a stale view.
func (e *Engine) HandleTimeout(ctx context.Context, targetBranch string, batchID int64) error {
	defer e.lock(targetBranch)()
	b, err := e.Queue.GetBatch(ctx, batchID)
	if err != nil || b == nil || b.State != pg.BatchStateTesting || !TimedOut(b, e.CheckTimeout) {
		return err
	}
	return e.HandleFail(ctx, b, "timeout", "")
}

// OnMemberRemoved drops an entry from the live batch (push/close/retarget/
// cancel). The caller is responsible for cancelling automerge / commenting on
// the PR; this function only adjusts batch bookkeeping and rebuilds when the
// removed entry was on the branch under test.
func (e *Engine) OnMemberRemoved(ctx context.Context, targetBranch string, batchID, entryID int64) error {
	defer e.lock(targetBranch)()
	b, err := e.Queue.GetBatch(ctx, batchID)
	if err != nil || b == nil {
		return err
	}
	if b.State != pg.BatchStateForming && b.State != pg.BatchStateTesting {
		return nil
	}
	id := entryID
	wasCurrent := slices.Contains(b.CurrentIds, id)
	b.CurrentIds = slices.DeleteFunc(b.CurrentIds, func(v int64) bool { return v == id })
	b.Pending = loadPending(b.Pending).drop(id).bytes()
	b.EjectedIds = append(b.EjectedIds, id)
	if !wasCurrent {
		return e.Queue.SaveBatch(ctx, b)
	}
	if len(b.CurrentIds) == 0 {
		return e.next(ctx, b)
	}
	return e.rebuild(ctx, b)
}

// TimedOut reports whether the batch has been testing longer than timeout.
func TimedOut(b *pg.Batch, timeout time.Duration) bool {
	return timeout > 0 && b.TestingStartedAt.Valid &&
		time.Since(b.TestingStartedAt.Time) > timeout
}

// LiveBranchNames returns the branch name of every live batch for the repo so
// the orphan-branch reaper can spare them.
func (e *Engine) LiveBranchNames(ctx context.Context) ([]string, error) {
	bs, err := e.Queue.ListLiveBatches(ctx, e.RepoID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		if b.BranchName.Valid {
			out = append(out, b.BranchName.String)
		}
	}
	return out, nil
}

// ReconcileLive resumes batches found at startup: forming → rebuild;
// testing → re-sync each current entry's merge_branch_* from the batch row
// so a crash between Build's SaveBatch and SetMergeBranch cannot strand the
// batch with no entry routing the current SHA.
func (e *Engine) ReconcileLive(ctx context.Context) error {
	bs, err := e.Queue.ListLiveBatches(ctx, e.RepoID)
	if err != nil {
		return err
	}
	for i := range bs {
		b := &bs[i]
		unlock := e.lock(b.TargetBranch)
		switch b.State {
		case pg.BatchStateForming:
			slog.Info("batch reconcile: rebuilding forming batch", "batch", b.ID)
			if err := e.Build(ctx, b); err != nil {
				slog.Warn("batch reconcile build failed", "batch", b.ID, "err", err)
			}
		case pg.BatchStateTesting:
			entries, _ := e.Queue.GetEntriesByIDs(ctx, b.CurrentIds)
			for _, ent := range entries {
				_ = e.Queue.SetMergeBranch(ctx, e.RepoID, ent.PrNumber,
					b.BranchName.String, b.BranchSha.String)
			}
		}
		unlock()
	}
	return nil
}

// stack builds the batch branch, preferring a forge-native MergeStacker when
// available so Gitea uses one clone for the whole stack.
func (e *Engine) stack(ctx context.Context, base string, heads []string, branch string) (string, []forge.MergeStep, error) {
	if s, ok := e.Forge.(forge.MergeStacker); ok {
		return s.StackMerges(ctx, e.Owner, e.Repo, base, heads, branch)
	}
	steps := make([]forge.MergeStep, len(heads))
	var tip string
	for i, head := range heads {
		var (
			sha      string
			conflict bool
			err      error
		)
		if tip == "" {
			sha, conflict, err = e.Forge.CreateMergeBranch(ctx, e.Owner, e.Repo, base, head, branch)
		} else {
			sha, conflict, err = e.Forge.MergeInto(ctx, e.Owner, e.Repo, branch, head)
		}
		steps[i] = forge.MergeStep{Conflict: conflict, Err: err}
		if !conflict && err == nil {
			tip = sha
		}
	}
	return tip, steps, nil
}

// rebuild persists the batch as forming (so a crash before Build's own save
// is picked up by ReconcileLive, and a concurrent HandleCheck drops on the
// state guard) and then runs Build.
func (e *Engine) rebuild(ctx context.Context, b *pg.Batch) error {
	b.State = pg.BatchStateForming
	if err := e.Queue.SaveBatch(ctx, b); err != nil {
		return err
	}
	return e.Build(ctx, b)
}

// next pops the pending stack and rebuilds, or finishes the batch.
func (e *Engine) next(ctx context.Context, b *pg.Batch) error {
	pending := loadPending(b.Pending)
	if n := len(pending); n > 0 {
		b.CurrentIds = pending[n-1]
		b.Pending = pending[:n-1].bytes()
		return e.rebuild(ctx, b)
	}

	// Root failed but every member eventually landed and nobody was ejected →
	// flaky CI or a cross-PR interaction across halves.
	b.Flaky = len(b.EjectedIds) == 0 && b.Builds > 1 &&
		len(b.LandedIds) == len(b.MemberIds)
	b.State = pg.BatchStateDone
	b.CurrentIds = nil
	if err := e.Queue.SaveBatch(ctx, b); err != nil {
		return err
	}
	_ = e.Forge.DeleteBranch(ctx, e.Owner, e.Repo, b.BranchName.String)

	slog.Info("batch done", "batch", b.ID, "landed", len(b.LandedIds),
		"ejected", len(b.EjectedIds), "builds", b.Builds, "flaky", b.Flaky)
	if e.Advance != nil {
		e.Advance()
	}
	return nil
}

// eject removes a single member: status, cancel automerge, comment, dequeue.
func (e *Engine) eject(ctx context.Context, b *pg.Batch, ent *pg.QueueEntry, state pg.CheckState, statusDesc, comment string) {
	_ = e.Forge.SetMQStatus(ctx, e.Owner, e.Repo, ent.PrHeadSha, forge.MQStatus{
		State: state, Description: statusDesc, TargetURL: e.prURL(ent.PrNumber),
	})
	_ = e.Forge.CancelAutoMerge(ctx, e.Owner, e.Repo, ent.PrNumber)
	_ = e.Forge.Comment(ctx, e.Owner, e.Repo, ent.PrNumber, comment)
	if _, err := e.Queue.Dequeue(ctx, e.RepoID, ent.PrNumber); err != nil {
		slog.Warn("dequeue ejected PR failed", "pr", ent.PrNumber, "err", err)
	}
	b.EjectedIds = append(b.EjectedIds, ent.ID)
}

func (e *Engine) ejectCurrent(ctx context.Context, b *pg.Batch, state pg.CheckState, statusDesc, comment string) {
	entries, _ := e.Queue.GetEntriesByIDs(ctx, b.CurrentIds)
	for i := range entries {
		e.eject(ctx, b, &entries[i], state, statusDesc, comment)
	}
	b.CurrentIds = nil
}

// ensureMergedOrClose waits briefly for the forge to notice the PR's head is
// reachable from the target branch. If it does not, the PR is closed with a
// comment carrying the landing SHA. The forge's merge endpoint is never
// called: that could mint a second merge commit on top of the tested SHA.
func (e *Engine) ensureMergedOrClose(ctx context.Context, ent *pg.QueueEntry, sha string, batchID int64) {
	interval := cmp.Or(e.MergedPollInterval, 200*time.Millisecond)
	for range cmp.Or(e.MergedPollAttempts, 50) {
		pr, err := e.Forge.GetPR(ctx, e.Owner, e.Repo, ent.PrNumber)
		if err == nil && pr != nil && (pr.Merged || pr.State == "closed") {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
	_ = e.Forge.Comment(ctx, e.Owner, e.Repo, ent.PrNumber,
		fmt.Sprintf("✅ Merged as `%s` via batch #%d.", sha, batchID))
	_ = e.Forge.ClosePR(ctx, e.Owner, e.Repo, ent.PrNumber)
}

func (e *Engine) prURL(n int64) string {
	return forge.DashboardPRURL(e.ExternalURL, e.Forge.Kind(), e.Owner, e.Repo, n)
}

// MemberBucket is a member's position within a live batch for UI display.
type MemberBucket string

const (
	BucketCurrent MemberBucket = "current"
	BucketPending MemberBucket = "pending"
	BucketLanded  MemberBucket = "landed"
	BucketEjected MemberBucket = "ejected"
)

// Bucket reports which slice of b the entry id is in.
func Bucket(b *pg.Batch, id int64) MemberBucket {
	switch {
	case slices.Contains(b.CurrentIds, id):
		return BucketCurrent
	case slices.Contains(b.LandedIds, id):
		return BucketLanded
	case slices.Contains(b.EjectedIds, id):
		return BucketEjected
	default:
		for _, s := range loadPending(b.Pending) {
			if slices.Contains(s, id) {
				return BucketPending
			}
		}
	}
	return ""
}

func ids(entries []pg.QueueEntry) []int64 {
	out := make([]int64, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}
