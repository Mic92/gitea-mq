package batch_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/Mic92/gitea-mq/internal/batch"
	"github.com/Mic92/gitea-mq/internal/forge"
	"github.com/Mic92/gitea-mq/internal/queue"
	"github.com/Mic92/gitea-mq/internal/store/pg"
	"github.com/Mic92/gitea-mq/internal/testutil"
)

// fakeForge is a MockForge wired so CreateMergeBranch/MergeInto/FastForward
// behave like ghfake.hMerge: each merge encodes its parents, and a
// fast-forward is accepted iff the new SHA contains the current target tip.
type fakeForge struct {
	*forge.MockForge
	target   string // current tip of main
	conflict map[string]bool
	merged   map[int64]bool // PR -> reported merged by GetPR
}

func newFakeForge() *fakeForge {
	f := &fakeForge{
		MockForge: &forge.MockForge{},
		target:    "main0",
		conflict:  map[string]bool{},
		merged:    map[int64]bool{},
	}
	var tip string
	f.CreateMergeBranchFn = func(_ context.Context, _, _, _, head, _ string) (string, bool, error) {
		if f.conflict[head] {
			return "", true, nil
		}
		// Seed from the live target tip so each rebuild reflects what
		// HandlePass already landed.
		tip = fmt.Sprintf("m(%s,%s)", f.target, head)
		return tip, false, nil
	}
	f.MergeIntoFn = func(_ context.Context, _, _, _, head string) (string, bool, error) {
		if f.conflict[head] {
			return "", true, nil
		}
		tip = fmt.Sprintf("m(%s,%s)", tip, head)
		return tip, false, nil
	}
	f.FastForwardFn = func(_ context.Context, _, _, _, sha string) error {
		if !strings.Contains(sha, f.target) {
			return forge.ErrNotFastForward
		}
		f.target = sha
		return nil
	}
	f.GetPRFn = func(_ context.Context, _, _ string, n int64) (*forge.PR, error) {
		return &forge.PR{Number: n, Merged: f.merged[n], State: "open"}, nil
	}
	return f
}

func setup(t *testing.T, prs ...int64) (*batch.Engine, *fakeForge, *queue.Service, context.Context) {
	t.Helper()
	svc, ctx, repoID := testutil.TestQueueService(t)
	for _, n := range prs {
		if _, err := svc.Enqueue(ctx, repoID, n, fmt.Sprintf("sha%d", n), "main"); err != nil {
			t.Fatal(err)
		}
	}
	f := newFakeForge()
	e := &batch.Engine{
		Forge:              f.MockForge,
		Queue:              svc,
		Owner:              "org",
		Repo:               "app",
		RepoID:             repoID,
		ExternalURL:        "http://mq",
		BatchMax:           0,
		MergedPollInterval: 1,
		MergedPollAttempts: 1,
	}
	return e, f, svc, ctx
}

func mustLive(t *testing.T, svc *queue.Service, ctx context.Context, repoID int64) *pg.Batch {
	t.Helper()
	b, err := svc.GetLiveBatch(ctx, repoID, "main")
	if err != nil {
		t.Fatal(err)
	}
	if b == nil {
		t.Fatal("expected live batch")
	}
	return b
}

func TestFormAndBuild_Green(t *testing.T) {
	e, f, svc, ctx := setup(t, 10, 20, 30)
	f.merged[10], f.merged[20], f.merged[30] = true, true, true

	b, err := e.FormAndBuild(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if b.State != pg.BatchStateTesting || len(b.CurrentIds) != 3 || b.Builds != 1 {
		t.Fatalf("unexpected batch after build: %+v", b)
	}
	wantSHA := "m(m(m(main0,sha10),sha20),sha30)"
	if b.BranchSha.String != wantSHA {
		t.Fatalf("branch sha = %q, want %q", b.BranchSha.String, wantSHA)
	}
	// All current entries route check events via the batch SHA.
	for _, n := range []int64{10, 20, 30} {
		ent, _ := svc.GetEntry(ctx, e.RepoID, n)
		if ent.MergeBranchSha.String != wantSHA || !ent.ActiveBatchID.Valid {
			t.Fatalf("PR #%d not wired to batch: %+v", n, ent)
		}
	}
	// Exactly one pending status per member, on the first build only.
	if got := len(f.CallsTo("SetMQStatus")); got != 3 {
		t.Fatalf("SetMQStatus calls = %d, want 3", got)
	}

	if err := e.HandlePass(ctx, b); err != nil {
		t.Fatal(err)
	}
	if f.target != wantSHA {
		t.Fatalf("target = %q, want fast-forward to %q", f.target, wantSHA)
	}
	if b.State != pg.BatchStateDone || len(b.LandedIds) != 3 || b.Flaky {
		t.Fatalf("unexpected batch after pass: %+v", b)
	}
	for _, n := range []int64{10, 20, 30} {
		if ent, _ := svc.GetEntry(ctx, e.RepoID, n); ent != nil {
			t.Fatalf("PR #%d still queued after landing", n)
		}
	}
	if len(f.CallsTo("ClosePR")) != 0 {
		t.Fatalf("ClosePR called despite forge reporting merged")
	}

	// FormAndBuild is a no-op on an empty queue.
	if b2, err := e.FormAndBuild(ctx, "main"); err != nil || b2 != nil {
		t.Fatalf("FormAndBuild on empty queue: b=%v err=%v", b2, err)
	}
}

func TestBuild_ConflictEjectsAndContinues(t *testing.T) {
	e, f, svc, ctx := setup(t, 10, 20, 30)
	f.conflict["sha20"] = true

	b, err := e.FormAndBuild(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(b.CurrentIds); got != 2 {
		t.Fatalf("current after conflict = %d, want 2", got)
	}
	if !slices.Contains(b.EjectedIds, b.MemberIds[1]) {
		t.Fatalf("member #20 not ejected: %+v", b)
	}
	if ent, _ := svc.GetEntry(ctx, e.RepoID, 20); ent != nil {
		t.Fatal("ejected PR #20 still queued")
	}
	if b.BranchSha.String != "m(m(main0,sha10),sha30)" {
		t.Fatalf("branch sha = %q", b.BranchSha.String)
	}
}

// TestBisect_CulpritAt2 walks the canonical 4-PR scenario: [10,20,30,40] with
// #20 failing. Expected: [10,20]f → [10]p land → [20]f eject → [30,40]p land.
func TestBisect_CulpritAt2(t *testing.T) {
	e, f, svc, ctx := setup(t, 10, 20, 30, 40)
	for _, n := range []int64{10, 30, 40} {
		f.merged[n] = true
	}

	b, err := e.FormAndBuild(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}

	// Build 1: [10,20,30,40] fails.
	if err := e.HandleFail(ctx, b, "ci/test", "http://ci/1"); err != nil {
		t.Fatal(err)
	}
	b = mustLive(t, svc, ctx, e.RepoID)
	if b.Builds != 2 || len(b.CurrentIds) != 2 {
		t.Fatalf("after first split: builds=%d current=%d", b.Builds, len(b.CurrentIds))
	}
	// #30,#40 are off the branch; their routing SHA must be cleared so a late
	// event for build-1's SHA cannot reach them.
	if ent, _ := svc.GetEntry(ctx, e.RepoID, 30); ent.MergeBranchSha.Valid {
		t.Fatal("pending member #30 still has merge_branch_sha")
	}

	// Build 2: [10,20] fails.
	if err := e.HandleFail(ctx, b, "ci/test", ""); err != nil {
		t.Fatal(err)
	}
	b = mustLive(t, svc, ctx, e.RepoID)
	if b.Builds != 3 || len(b.CurrentIds) != 1 {
		t.Fatalf("after second split: builds=%d current=%d", b.Builds, len(b.CurrentIds))
	}

	// Build 3: [10] passes → lands; pop [20].
	if err := e.HandlePass(ctx, b); err != nil {
		t.Fatal(err)
	}
	b = mustLive(t, svc, ctx, e.RepoID)
	if !strings.Contains(f.target, "sha10") {
		t.Fatalf("target after landing #10 = %q", f.target)
	}

	// Build 4: [20] fails → eject; pop [30,40].
	if err := e.HandleFail(ctx, b, "ci/test", "http://ci/4"); err != nil {
		t.Fatal(err)
	}
	if ent, _ := svc.GetEntry(ctx, e.RepoID, 20); ent != nil {
		t.Fatal("culprit #20 still queued")
	}
	b = mustLive(t, svc, ctx, e.RepoID)
	if len(b.CurrentIds) != 2 || b.Builds != 5 {
		t.Fatalf("after culprit eject: current=%d builds=%d", len(b.CurrentIds), b.Builds)
	}
	// Build 5 must start from the tip that already contains #10.
	if !strings.Contains(b.BranchSha.String, "sha10") {
		t.Fatalf("rebuild not on top of landed #10: %q", b.BranchSha.String)
	}

	// Build 5: [30,40] passes → done.
	if err := e.HandlePass(ctx, b); err != nil {
		t.Fatal(err)
	}
	if b.State != pg.BatchStateDone || len(b.LandedIds) != 3 || len(b.EjectedIds) != 1 || b.Flaky {
		t.Fatalf("final: %+v", b)
	}
	if !strings.Contains(f.target, "sha40") {
		t.Fatalf("final target = %q", f.target)
	}
	// Status discipline: 4 pending (build 1) + 4 terminal = 8.
	if got := len(f.CallsTo("SetMQStatus")); got != 8 {
		t.Fatalf("SetMQStatus calls = %d, want 8", got)
	}
}

func TestBisect_BothHalvesPass_Flaky(t *testing.T) {
	e, f, _, ctx := setup(t, 10, 20)
	f.merged[10], f.merged[20] = true, true

	b, _ := e.FormAndBuild(ctx, "main")
	if err := e.HandleFail(ctx, b, "ci/flaky", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.HandlePass(ctx, b); err != nil { // [10]
		t.Fatal(err)
	}
	if err := e.HandlePass(ctx, b); err != nil { // [20]
		t.Fatal(err)
	}
	if !b.Flaky || b.State != pg.BatchStateDone || len(b.LandedIds) != 2 {
		t.Fatalf("expected flaky done with 2 landed: %+v", b)
	}
}

// TestHandleCheck_EvaluatesAndDispatches drives the engine via the public
// monitor entry point so the lock + guard + save + evaluate path is covered.
func TestHandleCheck_EvaluatesAndDispatches(t *testing.T) {
	e, f, svc, ctx := setup(t, 10, 20)
	f.merged[10], f.merged[20] = true, true
	f.GetRequiredChecksFn = func(_ context.Context, _, _, _ string) ([]string, error) {
		return []string{"ci"}, nil
	}

	if _, err := e.FormAndBuild(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	rep, _ := svc.GetEntry(ctx, e.RepoID, 10)

	if err := e.HandleCheck(ctx, rep, "ci", pg.CheckStateSuccess, ""); err != nil {
		t.Fatal(err)
	}
	if b, _ := svc.GetLiveBatch(ctx, e.RepoID, "main"); b != nil {
		t.Fatalf("batch still live: %+v", b)
	}
	if !strings.Contains(f.target, "sha20") {
		t.Fatalf("target = %q", f.target)
	}
}

func TestHandlePass_NotFastForward_RetryThenCap(t *testing.T) {
	e, f, svc, ctx := setup(t, 10)
	f.FastForwardFn = func(_ context.Context, _, _, _, _ string) error {
		return forge.ErrNotFastForward
	}

	b, _ := e.FormAndBuild(ctx, "main")
	for range batch.MaxFFRetries - 1 {
		if err := e.HandlePass(ctx, b); err != nil {
			t.Fatal(err)
		}
		b = mustLive(t, svc, ctx, e.RepoID)
		if b.State != pg.BatchStateTesting {
			t.Fatalf("expected rebuild, state=%s retries=%d", b.State, b.FfRetries)
		}
	}
	if err := e.HandlePass(ctx, b); err != nil {
		t.Fatal(err)
	}
	if b.State != pg.BatchStateDone || len(b.EjectedIds) != 1 {
		t.Fatalf("expected eject after cap: %+v", b)
	}
}

func TestHandlePass_PushDenied(t *testing.T) {
	e, f, _, ctx := setup(t, 10, 20)
	f.FastForwardFn = func(_ context.Context, _, _, branch, _ string) error {
		return &forge.PushDeniedError{Branch: branch, Message: "user not in whitelist"}
	}

	b, _ := e.FormAndBuild(ctx, "main")
	if err := e.HandlePass(ctx, b); err != nil {
		t.Fatal(err)
	}
	if b.State != pg.BatchStateDone || len(b.EjectedIds) != 2 {
		t.Fatalf("expected both ejected: %+v", b)
	}
	var found bool
	for _, c := range f.CallsTo("Comment") {
		if strings.Contains(c.Args[3].(string), "push whitelist") {
			found = true
		}
	}
	if !found {
		t.Fatal("expected actionable whitelist hint in comment")
	}
}

func TestEnsureMergedOrClose_FallsBackToClose(t *testing.T) {
	e, f, _, ctx := setup(t, 10)
	// merged[10] left false → poll never sees it merged.

	b, _ := e.FormAndBuild(ctx, "main")
	if err := e.HandlePass(ctx, b); err != nil {
		t.Fatal(err)
	}
	if got := len(f.CallsTo("ClosePR")); got != 1 {
		t.Fatalf("ClosePR calls = %d, want 1", got)
	}
}

func TestOnMemberRemoved(t *testing.T) {
	e, _, svc, ctx := setup(t, 10, 20, 30, 40)
	b, _ := e.FormAndBuild(ctx, "main")

	// Split so #30,#40 are pending.
	if err := e.HandleFail(ctx, b, "ci", ""); err != nil {
		t.Fatal(err)
	}
	b = mustLive(t, svc, ctx, e.RepoID)
	shaBefore := b.BranchSha.String

	// Remove #40 (pending only) → no rebuild.
	ent40, _ := svc.GetEntry(ctx, e.RepoID, 40)
	if err := e.OnMemberRemoved(ctx, "main", b.ID, ent40.ID); err != nil {
		t.Fatal(err)
	}
	b = mustLive(t, svc, ctx, e.RepoID)
	if b.BranchSha.String != shaBefore || b.Builds != 2 {
		t.Fatalf("pending removal triggered rebuild: %+v", b)
	}

	// Remove #10 (current) → rebuild trimmed.
	ent10, _ := svc.GetEntry(ctx, e.RepoID, 10)
	if err := e.OnMemberRemoved(ctx, "main", b.ID, ent10.ID); err != nil {
		t.Fatal(err)
	}
	b = mustLive(t, svc, ctx, e.RepoID)
	if b.Builds != 3 || len(b.CurrentIds) != 1 {
		t.Fatalf("current removal did not rebuild: builds=%d current=%v", b.Builds, b.CurrentIds)
	}
}

func TestBisectMaxSteps(t *testing.T) {
	e, _, _, ctx := setup(t, 10, 20, 30, 40)
	e.BisectMaxSteps = 2

	b, _ := e.FormAndBuild(ctx, "main")
	if err := e.HandleFail(ctx, b, "ci", ""); err != nil { // build 1 → split, build 2
		t.Fatal(err)
	}
	if err := e.HandleFail(ctx, b, "ci", ""); err != nil { // build 2 fail → cap
		t.Fatal(err)
	}
	// Cap must drain pending too: exactly 2 builds, all 4 ejected, done.
	if b.State != pg.BatchStateDone || len(b.EjectedIds) != 4 || b.Builds != 2 {
		t.Fatalf("cap leaked builds or members: builds=%d ejected=%d state=%s",
			b.Builds, len(b.EjectedIds), b.State)
	}
}

func TestFormAndBuild_RespectsLive(t *testing.T) {
	e, _, _, ctx := setup(t, 10, 20)
	if _, err := e.FormAndBuild(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if b, err := e.FormAndBuild(ctx, "main"); err != nil || b != nil {
		t.Fatalf("second FormAndBuild should be a no-op: b=%v err=%v", b, err)
	}
}

// A transient Build error leaves the row forming; the next FormAndBuild must
// retry it instead of no-oping until restart.
func TestFormAndBuild_RetriesForming(t *testing.T) {
	e, f, svc, ctx := setup(t, 10, 20)

	want := errors.New("boom")
	var calls int
	e.Forge = stackerForge{Forge: f, fn: func() (string, []forge.MergeStep, error) {
		calls++
		if calls == 1 {
			return "", nil, want
		}
		return "tip", make([]forge.MergeStep, 2), nil
	}}
	if _, err := e.FormAndBuild(ctx, "main"); !errors.Is(err, want) {
		t.Fatalf("first build: err=%v", err)
	}
	live := mustLive(t, svc, ctx, e.RepoID)
	if live.State != pg.BatchStateForming {
		t.Fatalf("want forming after error, got %s", live.State)
	}
	if _, err := e.FormAndBuild(ctx, "main"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	live = mustLive(t, svc, ctx, e.RepoID)
	if live.State != pg.BatchStateTesting || calls != 2 {
		t.Fatalf("retry did not rebuild: state=%s calls=%d", live.State, calls)
	}
}

type stackerForge struct {
	forge.Forge
	fn func() (string, []forge.MergeStep, error)
}

func (s stackerForge) StackMerges(context.Context, string, string, string, []string, string) (string, []forge.MergeStep, error) {
	return s.fn()
}

func TestHandleCheck_DropsStaleSHA(t *testing.T) {
	// A late failure for a superseded build SHA must neither bisect the new
	// build nor pollute its check ledger.
	e, f, svc, ctx := setup(t, 10, 20)
	b, _ := e.FormAndBuild(ctx, "main")

	stale, _ := svc.GetEntry(ctx, e.RepoID, 10)

	// Simulate a target-branch move so the rebuild yields a different SHA.
	f.target = "main1"
	if err := e.Build(ctx, b); err != nil {
		t.Fatal(err)
	}
	if b.BranchSha.String == stale.MergeBranchSha.String {
		t.Fatalf("rebuild produced same sha %q", b.BranchSha.String)
	}

	if err := e.HandleCheck(ctx, stale, "ci", pg.CheckStateFailure, ""); err != nil {
		t.Fatal(err)
	}
	got := mustLive(t, svc, ctx, e.RepoID)
	if got.Builds != b.Builds {
		t.Fatalf("stale event mutated batch: builds %d → %d", b.Builds, got.Builds)
	}
	if cs, _ := svc.GetCheckStatuses(ctx, stale.ID); len(cs) != 0 {
		t.Fatalf("stale check persisted: %+v", cs)
	}
}

func TestPendingDrop(t *testing.T) {
	e, _, svc, ctx := setup(t, 10, 20, 30)
	b, _ := e.FormAndBuild(ctx, "main")
	if err := e.HandleFail(ctx, b, "ci", ""); err != nil {
		t.Fatal(err)
	}
	// pending now [[id20,id30]]; drop both → next() finishes the batch.
	for _, n := range []int64{20, 30} {
		ent, _ := svc.GetEntry(ctx, e.RepoID, n)
		if err := e.OnMemberRemoved(ctx, "main", b.ID, ent.ID); err != nil {
			t.Fatal(err)
		}
	}
	b = mustLive(t, svc, ctx, e.RepoID)
	if err := e.HandleFail(ctx, b, "ci", ""); err != nil { // singleton [10] fails
		t.Fatal(err)
	}
	if b.State != pg.BatchStateDone {
		t.Fatalf("expected done, got %+v", b)
	}
}
