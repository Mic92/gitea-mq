package queue_test

import (
	"testing"

	"github.com/jogman/gitea-mq/internal/queue"
	"github.com/jogman/gitea-mq/internal/store/pg"
	"github.com/jogman/gitea-mq/internal/testutil"
)

// Tests the core enqueue→head→advance→empty cycle that every PR goes through.
func TestEnqueueHeadAdvanceCycle(t *testing.T) {
	svc, ctx, repoID := testutil.TestQueueService(t)

	// Enqueue three PRs.
	r1, err := svc.Enqueue(ctx, repoID, 10, "sha10", "main")
	if err != nil {
		t.Fatal(err)
	}

	if !r1.IsNew || r1.Position != 1 {
		t.Fatalf("first enqueue: IsNew=%v Position=%d", r1.IsNew, r1.Position)
	}

	for _, pr := range []int64{20, 30} {
		if _, err := svc.Enqueue(ctx, repoID, pr, "sha", "main"); err != nil {
			t.Fatal(err)
		}
	}

	// Head is the first-enqueued PR.
	head, err := svc.Head(ctx, repoID, "main")
	if err != nil {
		t.Fatal(err)
	}

	if head.PrNumber != 10 {
		t.Fatalf("expected head PR #10, got #%d", head.PrNumber)
	}

	// Advance removes head, returns next.
	next, err := svc.Advance(ctx, repoID, "main")
	if err != nil {
		t.Fatal(err)
	}

	if next.PrNumber != 20 {
		t.Fatalf("expected next PR #20, got #%d", next.PrNumber)
	}

	// Advance twice more to empty.
	if _, err := svc.Advance(ctx, repoID, "main"); err != nil {
		t.Fatal(err)
	}

	empty, err := svc.Advance(ctx, repoID, "main")
	if err != nil {
		t.Fatal(err)
	}

	if empty != nil {
		t.Fatal("expected nil after advancing past last entry")
	}
}

// Enqueueing a duplicate is a no-op — spec requires at-most-once.
func TestEnqueueDuplicateIsNoop(t *testing.T) {
	svc, ctx, repoID := testutil.TestQueueService(t)

	if _, err := svc.Enqueue(ctx, repoID, 42, "sha", "main"); err != nil {
		t.Fatal(err)
	}

	dup, err := svc.Enqueue(ctx, repoID, 42, "sha", "main")
	if err != nil {
		t.Fatal(err)
	}

	if dup.IsNew {
		t.Fatal("duplicate enqueue should not be new")
	}

	entries, _ := svc.List(ctx, repoID, "main")
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
}

// Dequeue reports whether the removed PR was head-of-queue, which drives
// merge branch cleanup decisions.
func TestDequeueReportsHeadStatus(t *testing.T) {
	svc, ctx, repoID := testutil.TestQueueService(t)

	for _, pr := range []int64{10, 20} {
		if _, err := svc.Enqueue(ctx, repoID, pr, "sha", "main"); err != nil {
			t.Fatal(err)
		}
	}

	// Dequeue non-head.
	r, err := svc.Dequeue(ctx, repoID, 20)
	if err != nil {
		t.Fatal(err)
	}

	if !r.Found || r.WasHead {
		t.Fatalf("non-head dequeue: Found=%v WasHead=%v", r.Found, r.WasHead)
	}

	// Dequeue head.
	r, err = svc.Dequeue(ctx, repoID, 10)
	if err != nil {
		t.Fatal(err)
	}

	if !r.Found || !r.WasHead {
		t.Fatalf("head dequeue: Found=%v WasHead=%v", r.Found, r.WasHead)
	}

	// Dequeue missing PR.
	r, err = svc.Dequeue(ctx, repoID, 999)
	if err != nil {
		t.Fatal(err)
	}

	if r.Found {
		t.Fatal("expected Found=false for missing PR")
	}
}

// Queues for different repos and different branches within the same repo
// must not interfere with each other.
func TestQueueIsolation(t *testing.T) {
	pool := testutil.TestDB(t)
	svc := queue.NewService(pool)
	ctx := t.Context()

	repoA, _ := svc.GetOrCreateRepo(ctx, "org", "app-a")
	repoB, _ := svc.GetOrCreateRepo(ctx, "org", "app-b")

	// Different repos.
	if _, err := svc.Enqueue(ctx, repoA.ID, 1, "sha", "main"); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Enqueue(ctx, repoB.ID, 2, "sha", "main"); err != nil {
		t.Fatal(err)
	}

	// Different branches in same repo.
	if _, err := svc.Enqueue(ctx, repoA.ID, 3, "sha", "release"); err != nil {
		t.Fatal(err)
	}

	// Advancing repo A / main should not affect anything else.
	if _, err := svc.Advance(ctx, repoA.ID, "main"); err != nil {
		t.Fatal(err)
	}

	bEntries, _ := svc.List(ctx, repoB.ID, "main")
	if len(bEntries) != 1 {
		t.Fatalf("repo B should be unaffected, got %d entries", len(bEntries))
	}

	releaseEntries, _ := svc.List(ctx, repoA.ID, "release")
	if len(releaseEntries) != 1 {
		t.Fatalf("release branch should be unaffected, got %d entries", len(releaseEntries))
	}
}

// Tests the full state lifecycle: queued → testing (with merge branch) →
// check statuses recorded → state transitions.
func TestStateLifecycleAndChecks(t *testing.T) {
	svc, ctx, repoID := testutil.TestQueueService(t)

	enq, err := svc.Enqueue(ctx, repoID, 42, "abc123", "main")
	if err != nil {
		t.Fatal(err)
	}

	// Transition to testing with merge branch.
	if err := svc.UpdateState(ctx, repoID, 42, pg.EntryStateTesting); err != nil {
		t.Fatal(err)
	}

	if err := svc.SetMergeBranch(ctx, repoID, 42, "mq/42", "mergesha"); err != nil {
		t.Fatal(err)
	}

	entry, _ := svc.GetEntry(ctx, repoID, 42)
	if entry.State != pg.EntryStateTesting {
		t.Fatalf("expected testing state, got %s", entry.State)
	}

	if entry.MergeBranchName.String != "mq/42" {
		t.Fatalf("expected merge branch mq/42, got %s", entry.MergeBranchName.String)
	}

	// Record check statuses — latest update wins.
	if err := svc.SaveCheckStatus(ctx, enq.Entry.ID, "ci/build", pg.CheckStatePending); err != nil {
		t.Fatal(err)
	}

	if err := svc.SaveCheckStatus(ctx, enq.Entry.ID, "ci/build", pg.CheckStateSuccess); err != nil {
		t.Fatal(err)
	}

	checks, _ := svc.GetCheckStatuses(ctx, enq.Entry.ID)
	if len(checks) != 1 || checks[0].State != pg.CheckStateSuccess {
		t.Fatalf("expected 1 check with state success, got %v", checks)
	}
}

// LoadActiveQueues must exclude terminal states so startup recovery only
// processes in-flight entries.
func TestLoadActiveQueuesExcludesTerminal(t *testing.T) {
	svc, ctx, repoID := testutil.TestQueueService(t)

	if _, err := svc.Enqueue(ctx, repoID, 10, "sha", "main"); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Enqueue(ctx, repoID, 20, "sha", "main"); err != nil {
		t.Fatal(err)
	}

	if err := svc.UpdateState(ctx, repoID, 20, pg.EntryStateFailed); err != nil {
		t.Fatal(err)
	}

	active, err := svc.LoadActiveQueues(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(active) != 1 || active[0].PrNumber != 10 {
		t.Fatalf("expected only PR #10 active, got %v", active)
	}
}
