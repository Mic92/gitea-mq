package queue_test

import (
	"context"
	"testing"

	"github.com/Mic92/gitea-mq/internal/merge"
	"github.com/Mic92/gitea-mq/internal/queue"
	"github.com/Mic92/gitea-mq/internal/store/pg"
	"github.com/Mic92/gitea-mq/internal/testutil"
)

// Enqueueing a duplicate is a no-op — spec requires at-most-once.
func TestEnqueueDuplicateIsNoop(t *testing.T) {
	svc, ctx, repoID := testutil.TestQueueService(t)

	if _, err := svc.Enqueue(ctx, repoID, 42, "sha", "main", false); err != nil {
		t.Fatal(err)
	}

	dup, err := svc.Enqueue(ctx, repoID, 42, "sha", "main", false)
	if err != nil {
		t.Fatal(err)
	}

	if dup.IsNew {
		t.Fatal("duplicate enqueue should not be new")
	}

	entries, _ := svc.ListActiveEntries(ctx, repoID)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
}

// Queues for different repos and different branches within the same repo
// must not interfere with each other.
func TestQueueIsolation(t *testing.T) {
	pool := testutil.TestDB(t)
	svc := queue.NewService(pool)
	ctx := t.Context()

	repoA, _ := svc.GetOrCreateRepo(ctx, "gitea", "org", "app-a")
	repoB, _ := svc.GetOrCreateRepo(ctx, "gitea", "org", "app-b")

	// Different repos.
	if _, err := svc.Enqueue(ctx, repoA.ID, 1, "sha", "main", false); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Enqueue(ctx, repoB.ID, 2, "sha", "main", false); err != nil {
		t.Fatal(err)
	}

	// Different branches in same repo.
	if _, err := svc.Enqueue(ctx, repoA.ID, 3, "sha", "release", false); err != nil {
		t.Fatal(err)
	}

	// Each queue has its own head and positions.
	for _, c := range []struct {
		repoID int64
		branch string
		want   int64
	}{{repoA.ID, "main", 1}, {repoB.ID, "main", 2}, {repoA.ID, "release", 3}} {
		head, err := svc.Head(ctx, c.repoID, c.branch)
		if err != nil || head == nil || head.PrNumber != c.want {
			t.Fatalf("head(%d, %s) = %v, %v; want PR #%d", c.repoID, c.branch, head, err, c.want)
		}
		if pos, err := svc.Position(ctx, c.repoID, c.branch, c.want); err != nil || pos != 1 {
			t.Fatalf("position(%d, %s, #%d) = %d, %v; want 1", c.repoID, c.branch, c.want, pos, err)
		}
	}
}

// Tests the full state lifecycle: queued → testing (with merge branch) →
// check statuses recorded → state transitions.
func TestStateLifecycleAndChecks(t *testing.T) {
	svc, ctx, repoID := testutil.TestQueueService(t)

	enq, err := svc.Enqueue(ctx, repoID, 42, "abc123", "main", false)
	if err != nil {
		t.Fatal(err)
	}

	// Transition to testing with merge branch.
	if err := svc.UpdateState(ctx, repoID, 42, pg.EntryStateTesting); err != nil {
		t.Fatal(err)
	}

	wantBranch := merge.BranchName(42)

	if err := svc.SetMergeBranch(ctx, repoID, 42, wantBranch, "mergesha"); err != nil {
		t.Fatal(err)
	}

	entry, _ := svc.GetEntry(ctx, repoID, 42)
	if entry.State != pg.EntryStateTesting {
		t.Fatalf("expected testing state, got %s", entry.State)
	}

	if entry.MergeBranchName.String != wantBranch {
		t.Fatalf("expected merge branch %s, got %s", wantBranch, entry.MergeBranchName.String)
	}

	// Record check statuses — latest update wins.
	if err := svc.SaveCheckStatus(ctx, enq.Entry.ID, "ci/build", pg.CheckStatePending, ""); err != nil {
		t.Fatal(err)
	}

	if err := svc.SaveCheckStatus(ctx, enq.Entry.ID, "ci/build", pg.CheckStateSuccess, "https://ci.example.com/build/1"); err != nil {
		t.Fatal(err)
	}

	checks, _ := svc.GetCheckStatuses(ctx, enq.Entry.ID)
	if len(checks) != 1 || checks[0].State != pg.CheckStateSuccess {
		t.Fatalf("expected 1 check with state success, got %v", checks)
	}
	// Upsert must also update target_url.
	if checks[0].TargetUrl != "https://ci.example.com/build/1" {
		t.Fatalf("expected target_url from latest upsert, got %q", checks[0].TargetUrl)
	}
}

// Same owner/name on different forges must yield distinct repo rows so their
// queues are independent.
func TestGetOrCreateRepo_ForgeDisambiguates(t *testing.T) {
	pool := testutil.TestDB(t)
	svc := queue.NewService(pool)
	ctx := context.Background()

	a, err := svc.GetOrCreateRepo(ctx, "gitea", "org", "app")
	if err != nil {
		t.Fatalf("gitea: %v", err)
	}
	b, err := svc.GetOrCreateRepo(ctx, "github", "org", "app")
	if err != nil {
		t.Fatalf("github: %v", err)
	}
	if a.ID == b.ID {
		t.Fatalf("expected distinct rows, both got id=%d", a.ID)
	}

	// Idempotent on the (forge, owner, name) tuple.
	a2, err := svc.GetOrCreateRepo(ctx, "gitea", "org", "app")
	if err != nil {
		t.Fatal(err)
	}
	if a2.ID != a.ID {
		t.Fatalf("idempotent: got %d, want %d", a2.ID, a.ID)
	}
}
