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
	"github.com/Mic92/gitea-mq/internal/logutil"
	"github.com/Mic92/gitea-mq/internal/merge"
	"github.com/Mic92/gitea-mq/internal/metrics"
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
	MergeLabel     string
	// SkipIfUpToDate lands a single up-to-date PR by fast-forwarding to its
	// head instead of building a batch branch.
	SkipIfUpToDate bool

	// MergedPoll controls ensureMergedOrClose. Defaults: 200ms × 50 = 10s.
	MergedPollInterval time.Duration
	MergedPollAttempts int

	// StackMergedPoll controls the wait after a stack merge, which GitHub
	// finishes in the background. Defaults: 1s × (30 + 3 per member).
	StackMergedPollInterval time.Duration
	StackMergedPollAttempts int

	// Advance is called whenever a batch finishes so the next root can be
	// formed without waiting for the poll tick. Optional.
	Advance func()

	mu    sync.Mutex // guards locks and landedStacks
	locks map[string]*sync.Mutex
	// landedStacks holds stack tops handed to the forge's async stack merge,
	// which stay open and labeled until it finishes.
	landedStacks map[int64]landedStack
}

type landedStack struct {
	headSHA string
	at      time.Time
}

const (
	// GitHub merges a stack member by member: 40 members took about 90s.
	stackMergedBaseAttempts      = 30
	stackMergedAttemptsPerMember = 3

	// After this long an unfinished stack merge has most likely failed, so
	// the PR may be enqueued again.
	landedStackTTL = 10 * time.Minute
)

// RecentlyLanded reports whether the PR's head was handed to the forge's
// stack merge within landedStackTTL.
func (e *Engine) RecentlyLanded(prNumber int64, headSHA string) bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	l, ok := e.landedStacks[prNumber]
	if !ok {
		return false
	}
	if l.headSHA != headSHA || time.Since(l.at) > landedStackTTL {
		delete(e.landedStacks, prNumber)
		return false
	}
	return true
}

func (e *Engine) rememberLanded(prNumber int64, headSHA string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.landedStacks == nil {
		e.landedStacks = make(map[int64]landedStack)
	}
	e.landedStacks[prNumber] = landedStack{headSHA: headSHA, at: time.Now()}
}

// lock returns the per-target-branch unlock func. Batches for different
// branches in the same repo are independent (ux_batches_live is per branch),
// so serialising them on one mutex would head-of-line block on forge I/O.
func (e *Engine) lock(targetBranch string) func() {
	e.mu.Lock()
	if e.locks == nil {
		e.locks = make(map[string]*sync.Mutex)
	}
	l, ok := e.locks[targetBranch]
	if !ok {
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
	entries, err = e.deferStackTops(ctx, b, entries)
	if err != nil {
		return fmt.Errorf("resolve stacks: %w", err)
	}

	heads := make([]string, len(entries))
	for i := range entries {
		heads[i] = entries[i].PrHeadSha
	}
	tip, steps := e.headIfUpToDate(ctx, b.TargetBranch, heads)
	if tip == "" {
		tip, steps, err = e.stack(ctx, b.TargetBranch, heads, branch)
	}
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
			e.eject(ctx, b, ent, metrics.ReasonConflict, pg.CheckStateFailure, msg,
				"❌ Removed from merge queue: "+msg+". Please rebase and re-schedule automerge.")
		case step.Err != nil:
			e.eject(ctx, b, ent, metrics.ReasonError, pg.CheckStateError, "Failed to create merge branch",
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
	if err := e.Queue.SaveBatch(ctx, b, b.EjectedIds...); err != nil {
		return err
	}

	// Route check events: every current entry carries the batch branch/sha.
	// Clearing the check ledger comes last so a stale SaveCheckStatus that
	// raced in before SetMergeBranch (still keyed on the old SHA) is wiped.
	desc := fmt.Sprintf("Testing batch #%d (%d PRs)", b.ID, len(b.MemberIds))
	for _, ent := range surv {
		logutil.WarnIfErr(e.Queue.SetMergeBranch(ctx, e.RepoID, ent.PrNumber, branch, tip), "set merge branch failed", "pr", ent.PrNumber)
		if first {
			logutil.WarnIfErr(e.Forge.SetMQStatus(ctx, e.Owner, e.Repo, ent.PrHeadSha, forge.MQStatus{
				State: pg.CheckStatePending, Description: desc, TargetURL: e.prURL(ent.PrNumber),
			}), "set mq status failed", "pr", ent.PrNumber)
		}
	}
	logutil.WarnIfErr(e.Queue.ClearCheckStatuses(ctx, b.CurrentIds), "clear check statuses failed", "batch", b.ID)

	metrics.BatchBuilt(e.Forge.Kind(), e.Owner, e.Repo)
	slog.Info("batch built", "batch", b.ID, "build", b.Builds, "sha", tip,
		"current", len(surv), "pending", len(loadPending(b.Pending)))
	return nil
}

// isStackTop reports whether ent tops a stack with at least two unmerged
// members. A lone bottom PR targets the branch itself, so the forge marks it
// merged after a fast-forward.
func (e *Engine) isStackTop(ctx context.Context, ent *pg.QueueEntry) (bool, error) {
	stack, err := forge.ResolveStack(ctx, e.Forge, e.Owner, e.Repo, ent.PrNumber)
	if err != nil || stack == nil {
		return false, err
	}
	members, ok := stack.MembersUpTo(ent.PrNumber)
	if !ok {
		return false, nil
	}
	unmerged := 0
	for _, m := range members {
		if !m.Merged {
			unmerged++
		}
	}
	return unmerged >= 2, nil
}

// deferStackTops keeps stack tops out of shared batches, since a stack lands
// through the forge's merge and not by fast-forward to a batch SHA. Entries
// before the first stack top stay current, or the stack top alone when it
// comes first; the rest is pushed onto Pending in queue order.
func (e *Engine) deferStackTops(ctx context.Context, b *pg.Batch, entries []pg.QueueEntry) ([]pg.QueueEntry, error) {
	if len(entries) < 2 {
		return entries, nil
	}
	cut := len(entries)
	for i := range entries {
		top, err := e.isStackTop(ctx, &entries[i])
		if err != nil {
			return nil, err
		}
		if top {
			cut = max(i, 1)
			break
		}
	}
	if cut == len(entries) {
		return entries, nil
	}
	rest := ids(entries[cut:])
	b.Pending = append(loadPending(b.Pending), rest).bytes()
	b.CurrentIds = ids(entries[:cut])
	// Late check events for this build's SHA must not reach deferred entries.
	logutil.WarnIfErr(e.Queue.ClearMergeBranch(ctx, rest), "clear merge branch failed", "batch", b.ID)
	slog.Info("batch deferred stack tops", "batch", b.ID, "current", cut, "deferred", len(rest))
	return entries[:cut], nil
}

type landedStatusHead struct {
	sha      string
	prNumber int64
}

// landedStatusHeads expands queued stack tips before the target branch moves.
// GitHub no longer reliably resolves the stack after its members are merged.
func (e *Engine) landedStatusHeads(ctx context.Context, entries []pg.QueueEntry) ([]landedStatusHead, error) {
	var heads []landedStatusHead
	seen := make(map[string]bool)
	add := func(sha string, prNumber int64) {
		if sha == "" || seen[sha] {
			return
		}
		seen[sha] = true
		heads = append(heads, landedStatusHead{sha: sha, prNumber: prNumber})
	}

	for i := range entries {
		ent := &entries[i]
		stack, err := forge.ResolveStack(ctx, e.Forge, e.Owner, e.Repo, ent.PrNumber)
		if err != nil {
			return nil, fmt.Errorf("resolve landed stack for PR #%d: %w", ent.PrNumber, err)
		}
		if stack != nil {
			if members, ok := stack.MembersUpTo(ent.PrNumber); ok {
				for _, member := range members {
					add(member.HeadSHA, ent.PrNumber)
				}
			}
		}
		add(ent.PrHeadSha, ent.PrNumber)
	}
	return heads, nil
}

// HandlePass lands current_ids by fast-forwarding the target branch to the
// tested SHA, then pops the next pending slice.
func (e *Engine) HandlePass(ctx context.Context, b *pg.Batch) error {
	if b.State != pg.BatchStateTesting {
		return nil
	}
	entries, err := e.Queue.GetEntriesByIDs(ctx, b.CurrentIds)
	if err != nil {
		return err
	}
	// The fast-forward below skips the forge's dependency and review checks.
	held, err := e.holdBlocked(ctx, b, entries)
	if err != nil {
		return err
	}
	if held {
		if len(b.CurrentIds) == 0 {
			return e.next(ctx, b)
		}
		return e.rebuild(ctx, b)
	}
	if len(entries) == 1 {
		top, err := e.isStackTop(ctx, &entries[0])
		if err != nil {
			return err
		}
		if top {
			return e.landStack(ctx, b, &entries[0])
		}
	}
	statusHeads, err := e.landedStatusHeads(ctx, entries)
	if err != nil {
		return err
	}

	sha := b.BranchSha.String
	if err := e.Forge.FastForward(ctx, e.Owner, e.Repo, b.TargetBranch, sha); err != nil {
		var denied *forge.PushDeniedError
		switch {
		case errors.Is(err, forge.ErrNotFastForward):
			if e.retryMoved(ctx, b, "during fast-forward") {
				return e.rebuild(ctx, b)
			}
		case errors.As(err, &denied):
			e.ejectCurrent(ctx, b, metrics.ReasonError, pg.CheckStateError, "gitea-mq cannot push to "+b.TargetBranch,
				fmt.Sprintf("⚠️ Removed from merge queue: gitea-mq is not allowed to push to `%s`.\n\n"+
					"Add the gitea-mq user/app to the branch's push whitelist (or ruleset bypass) and re-schedule.\n\n```\n%s\n```",
					b.TargetBranch, denied.Message))
		default:
			return fmt.Errorf("fast-forward %s: %w", b.TargetBranch, err)
		}
		return e.next(ctx, b)
	}
	b.FfRetries = 0

	// Persist before the best-effort forge work: the target already moved.
	landed := b.CurrentIds
	b.LandedIds = append(b.LandedIds, landed...)
	b.CurrentIds = nil
	build := popNext(b)
	if err := e.Queue.SaveBatch(ctx, b, slices.Concat(landed, b.EjectedIds)...); err != nil {
		return err
	}
	slog.Info("batch landed", "batch", b.ID, "sha", sha, "prs", len(entries))
	for i := range entries {
		metrics.Dequeued(e.Forge.Kind(), e.Owner, e.Repo, &entries[i], metrics.ReasonMerged)
	}

	desc := fmt.Sprintf("Merged via batch #%d", b.ID)
	for _, head := range statusHeads {
		logutil.WarnIfErr(e.Forge.SetMQStatus(ctx, e.Owner, e.Repo, head.sha, forge.MQStatus{
			State: pg.CheckStateSuccess, Description: desc, TargetURL: e.prURL(head.prNumber),
		}), "set mq status failed", "pr", head.prNumber)
		merge.SkipPendingMirroredChecks(ctx, e.Forge, e.Owner, e.Repo, head.sha)
	}

	var wg sync.WaitGroup
	for i := range entries {
		ent := &entries[i]
		wg.Go(func() { e.ensureMergedOrClose(ctx, ent, sha, b.ID) })
	}
	wg.Wait()

	return e.finish(ctx, b, build)
}

// retryMoved counts a landing that found the target moved. It reports
// whether to rebuild; once retries run out the current members are ejected.
func (e *Engine) retryMoved(ctx context.Context, b *pg.Batch, when string) bool {
	b.FfRetries++
	if b.FfRetries < MaxFFRetries {
		slog.Info("batch landing raced; rebuilding", "batch", b.ID, "retry", b.FfRetries)
		return true
	}
	e.ejectCurrent(ctx, b, metrics.ReasonError, pg.CheckStateError, "Target branch moved repeatedly",
		fmt.Sprintf("⚠️ Removed from merge queue: the target branch moved %s "+
			"%d times in a row — is something else pushing to `%s`?", when, MaxFFRetries, b.TargetBranch))
	return false
}

// landStack lands a stack top through the forge's stack merge (GitHub
// merge-async). A fast-forward would land the commits but leave every member
// above the bottom open: their bases are stack branches, which never move.
// The forge re-checks gitea-mq on every member, so all are set first.
func (e *Engine) landStack(ctx context.Context, b *pg.Batch, ent *pg.QueueEntry) error {
	// The forge merges onto the current target tip, which must be the tested one.
	up, err := e.Forge.IsUpToDate(ctx, e.Owner, e.Repo, b.TargetBranch, b.BranchSha.String)
	if err != nil {
		return fmt.Errorf("check target tip of %s: %w", b.TargetBranch, err)
	}
	if !up {
		if e.retryMoved(ctx, b, "before the stack merge") {
			return e.rebuild(ctx, b)
		}
		return e.next(ctx, b)
	}

	statusHeads, err := e.landedStatusHeads(ctx, []pg.QueueEntry{*ent})
	if err != nil {
		return err
	}
	setAll := func(st forge.MQStatus) {
		for _, head := range statusHeads {
			st.TargetURL = e.prURL(head.prNumber)
			logutil.WarnIfErr(e.Forge.SetMQStatus(ctx, e.Owner, e.Repo, head.sha, st),
				"set mq status failed", "pr", head.prNumber)
		}
	}
	setAll(forge.MQStatus{State: pg.CheckStateSuccess, Description: fmt.Sprintf("Merging stack via batch #%d", b.ID)})
	// Lower members were never queued, so only the top has mirrored checks.
	merge.SkipPendingMirroredChecks(ctx, e.Forge, e.Owner, e.Repo, ent.PrHeadSha)

	if err := e.Forge.MergePR(ctx, e.Owner, e.Repo, ent.PrNumber); err != nil {
		setAll(forge.MQStatus{State: pg.CheckStateError, Description: "Stack merge failed"})
		e.eject(ctx, b, ent, metrics.ReasonError, pg.CheckStateError, "Stack merge failed",
			fmt.Sprintf("❌ Removed from merge queue: the forge refused to merge the stack.\n\n```\n%v\n```\n\n"+
				"Check the required checks and rules of every PR in the stack, then re-schedule.", err))
		b.CurrentIds = nil
		return e.next(ctx, b)
	}
	b.FfRetries = 0
	e.rememberLanded(ent.PrNumber, ent.PrHeadSha)

	landed := b.CurrentIds
	b.LandedIds = append(b.LandedIds, landed...)
	b.CurrentIds = nil
	build := popNext(b)
	if err := e.Queue.SaveBatch(ctx, b, slices.Concat(landed, b.EjectedIds)...); err != nil {
		return err
	}
	slog.Info("batch landed", "batch", b.ID, "sha", b.BranchSha.String, "prs", 1, "via", "stack merge", "pr", ent.PrNumber)
	metrics.Dequeued(e.Forge.Kind(), e.Owner, e.Repo, ent, metrics.ReasonMerged)

	// The next batch would race GitHub's merge on the target, so wait for it;
	// unlike ensureMergedOrClose, never close, which would race it too.
	attempts := cmp.Or(e.StackMergedPollAttempts, stackMergedBaseAttempts+stackMergedAttemptsPerMember*len(statusHeads))
	if !e.waitMerged(ctx, ent.PrNumber, cmp.Or(e.StackMergedPollInterval, time.Second), attempts) {
		slog.Warn("stack merge not reported by forge yet", "batch", b.ID, "pr", ent.PrNumber)
	}
	return e.finish(ctx, b, build)
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
			e.eject(ctx, b, &entries[0], metrics.ReasonCheckFailed, pg.CheckStateFailure,
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
		e.ejectCurrent(ctx, b, metrics.ReasonError, pg.CheckStateError,
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
	logutil.WarnIfErr(e.Queue.ClearMergeBranch(ctx, right), "clear merge branch failed", "batch", b.ID)
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
		return e.Queue.SaveBatch(ctx, b, b.EjectedIds...)
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
				logutil.WarnIfErr(e.Queue.SetMergeBranch(ctx, e.RepoID, ent.PrNumber,
					b.BranchName.String, b.BranchSha.String), "set merge branch failed", "pr", ent.PrNumber)
			}
		}
		unlock()
	}
	return nil
}

// headIfUpToDate returns the lone head as tip when it already contains base;
// empty tip means a batch branch must be built.
func (e *Engine) headIfUpToDate(ctx context.Context, base string, heads []string) (string, []forge.MergeStep) {
	if !e.SkipIfUpToDate || len(heads) != 1 {
		return "", nil
	}
	up, err := e.Forge.IsUpToDate(ctx, e.Owner, e.Repo, base, heads[0])
	if err != nil {
		slog.Warn("up-to-date check failed, building batch branch", "sha", heads[0], "error", err)
		return "", nil
	}
	if !up {
		return "", nil
	}
	return heads[0], []forge.MergeStep{{}}
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
	if err := e.Queue.SaveBatch(ctx, b, b.EjectedIds...); err != nil {
		return err
	}
	return e.Build(ctx, b)
}

// popNext pops the pending stack into current or marks the batch done.
// Returns true when a build is needed.
func popNext(b *pg.Batch) bool {
	pending := loadPending(b.Pending)
	if n := len(pending); n > 0 {
		b.CurrentIds = pending[n-1]
		b.Pending = pending[:n-1].bytes()
		b.State = pg.BatchStateForming
		return true
	}
	// Root failed but every member eventually landed and nobody was ejected →
	// flaky CI or a cross-PR interaction across halves.
	b.Flaky = len(b.EjectedIds) == 0 && b.Builds > 1 &&
		len(b.LandedIds) == len(b.MemberIds)
	b.State = pg.BatchStateDone
	b.CurrentIds = nil
	return false
}

// finish builds the popped slice or cleans up the finished batch; forming
// rows are retried by FormAndBuild.
func (e *Engine) finish(ctx context.Context, b *pg.Batch, build bool) error {
	if build {
		return e.Build(ctx, b)
	}
	logutil.WarnIfErr(e.Forge.DeleteBranch(ctx, e.Owner, e.Repo, b.BranchName.String), "delete batch branch failed", "branch", b.BranchName.String)
	slog.Info("batch done", "batch", b.ID, "landed", len(b.LandedIds),
		"ejected", len(b.EjectedIds), "builds", b.Builds, "flaky", b.Flaky)
	if e.Advance != nil {
		e.Advance()
	}
	return nil
}

// next pops the pending stack and rebuilds, or finishes the batch.
func (e *Engine) next(ctx context.Context, b *pg.Batch) error {
	build := popNext(b)
	if err := e.Queue.SaveBatch(ctx, b, b.EjectedIds...); err != nil {
		return err
	}
	return e.finish(ctx, b, build)
}

// eject marks a single member for removal: status, cancel automerge, comment.
func (e *Engine) eject(ctx context.Context, b *pg.Batch, ent *pg.QueueEntry, reason metrics.Reason, state pg.CheckState, statusDesc, comment string) {
	logutil.WarnIfErr(e.Forge.SetMQStatus(ctx, e.Owner, e.Repo, ent.PrHeadSha, forge.MQStatus{
		State: state, Description: statusDesc, TargetURL: e.prURL(ent.PrNumber),
	}), "set mq status failed", "pr", ent.PrNumber)
	merge.SkipPendingMirroredChecks(ctx, e.Forge, e.Owner, e.Repo, ent.PrHeadSha)
	logutil.WarnIfErr(forge.CancelMergeIntent(ctx, e.Forge, e.Owner, e.Repo, ent.PrNumber, e.MergeLabel), "cancel merge intent failed", "pr", ent.PrNumber)
	logutil.WarnIfErr(e.Forge.Comment(ctx, e.Owner, e.Repo, ent.PrNumber, comment), "post comment failed", "pr", ent.PrNumber)
	// The next SaveBatch deletes EjectedIds from the queue.
	b.EjectedIds = append(b.EjectedIds, ent.ID)
	metrics.Dequeued(e.Forge.Kind(), e.Owner, e.Repo, ent, reason)
}

func (e *Engine) ejectCurrent(ctx context.Context, b *pg.Batch, reason metrics.Reason, state pg.CheckState, statusDesc, comment string) {
	entries, _ := e.Queue.GetEntriesByIDs(ctx, b.CurrentIds)
	for i := range entries {
		e.eject(ctx, b, &entries[i], reason, state, statusDesc, comment)
	}
	b.CurrentIds = nil
}

// ensureMergedOrClose waits briefly for the forge to notice the PR's head is
// reachable from the target branch. If it does not, the PR is closed with a
// comment carrying the landing SHA. The forge's merge endpoint is never
// called: that could mint a second merge commit on top of the tested SHA.
func (e *Engine) ensureMergedOrClose(ctx context.Context, ent *pg.QueueEntry, sha string, batchID int64) {
	if e.waitMerged(ctx, ent.PrNumber, cmp.Or(e.MergedPollInterval, 200*time.Millisecond), cmp.Or(e.MergedPollAttempts, 50)) {
		return
	}
	if ctx.Err() != nil {
		return
	}
	logutil.WarnIfErr(e.Forge.Comment(ctx, e.Owner, e.Repo, ent.PrNumber,
		fmt.Sprintf("✅ Merged as `%s` via batch #%d.", sha, batchID)), "post comment failed", "pr", ent.PrNumber)
	logutil.WarnIfErr(e.Forge.ClosePR(ctx, e.Owner, e.Repo, ent.PrNumber), "close pr failed", "pr", ent.PrNumber)
}

// waitMerged polls until the PR is merged or closed, and reports whether it was.
func (e *Engine) waitMerged(ctx context.Context, number int64, interval time.Duration, attempts int) bool {
	for range attempts {
		pr, err := e.Forge.GetPR(ctx, e.Owner, e.Repo, number)
		if err == nil && pr != nil && (pr.Merged || pr.State == "closed") {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(interval):
		}
	}
	return false
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

// holdBlocked drops members the forge would not merge yet (open dependency,
// unmet review rules) but keeps their merge intent; the poller re-enqueues
// them once the hold clears. All lookups happen before any change, so a forge
// error leaves the batch untouched.
func (e *Engine) holdBlocked(ctx context.Context, b *pg.Batch, entries []pg.QueueEntry) (bool, error) {
	blocked := make(map[int64]*forge.Hold)
	for i := range entries {
		hold, err := forge.MergeHold(ctx, e.Forge, e.Owner, e.Repo, entries[i].PrNumber, entries[i].TargetBranch)
		if err != nil {
			return false, err
		}
		if hold != nil {
			blocked[entries[i].ID] = hold
		}
	}
	if len(blocked) == 0 {
		return false, nil
	}

	for i := range entries {
		ent := &entries[i]
		hold, ok := blocked[ent.ID]
		if !ok {
			continue
		}
		logutil.WarnIfErr(e.Forge.SetMQStatus(ctx, e.Owner, e.Repo, ent.PrHeadSha, forge.MQStatus{
			State: pg.CheckStatePending, Description: hold.Status, TargetURL: e.prURL(ent.PrNumber),
		}), "set mq status failed", "pr", ent.PrNumber)
		merge.SkipPendingMirroredChecks(ctx, e.Forge, e.Owner, e.Repo, ent.PrHeadSha)
		logutil.WarnIfErr(e.Forge.Comment(ctx, e.Owner, e.Repo, ent.PrNumber, hold.Comment),
			"post comment failed", "pr", ent.PrNumber)
		// The next SaveBatch deletes EjectedIds from the queue.
		b.EjectedIds = append(b.EjectedIds, ent.ID)
	}
	b.CurrentIds = slices.DeleteFunc(b.CurrentIds, func(id int64) bool {
		_, ok := blocked[id]
		return ok
	})
	slog.Info("batch members held back: forge would not merge them yet", "batch", b.ID, "held", len(blocked), "current", len(b.CurrentIds))
	return true, nil
}
