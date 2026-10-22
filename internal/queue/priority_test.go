package queue_test

import (
	"slices"
	"testing"

	"github.com/Mic92/gitea-mq/internal/queue"
	"github.com/Mic92/gitea-mq/internal/store/pg"
	"github.com/Mic92/gitea-mq/internal/testutil"
)

func enqueue(t *testing.T, svc *queue.Service, repoID int64, priority bool, prs ...int64) {
	t.Helper()
	for _, pr := range prs {
		if _, err := svc.Enqueue(t.Context(), repoID, pr, "sha", "main", priority); err != nil {
			t.Fatal(err)
		}
	}
}

func order(t *testing.T, svc *queue.Service, repoID int64) []int64 {
	t.Helper()
	entries, err := svc.ListActiveEntries(t.Context(), repoID)
	if err != nil {
		t.Fatal(err)
	}
	var prs []int64
	for i, e := range entries {
		prs = append(prs, e.PrNumber)
		pos, err := svc.Position(t.Context(), repoID, "main", e.PrNumber)
		if err != nil {
			t.Fatal(err)
		}
		if pos != int64(i+1) {
			t.Fatalf("PR #%d: position %d disagrees with list index %d", e.PrNumber, pos, i+1)
		}
	}
	return prs
}

// Priority PRs sort ahead of normal ones, FIFO within each class, but never
// ahead of an entry already in flight.
func TestPriorityOrdering(t *testing.T) {
	svc, ctx, repoID := testutil.TestQueueService(t)

	enqueue(t, svc, repoID, false, 10, 20)
	if err := svc.UpdateState(ctx, repoID, 10, pg.EntryStateTesting); err != nil {
		t.Fatal(err)
	}
	enqueue(t, svc, repoID, true, 30, 40)

	if got, want := order(t, svc, repoID), []int64{10, 30, 40, 20}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if head, _ := svc.Head(ctx, repoID, "main"); head == nil || head.PrNumber != 10 {
		t.Fatalf("head = %v, want testing PR #10", head)
	}

	// Re-ranking only affects queued entries.
	for _, pr := range []int64{20, 10} {
		if err := svc.SetPriority(ctx, repoID, pr, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.SetPriority(ctx, repoID, 30, false); err != nil {
		t.Fatal(err)
	}
	if got, want := order(t, svc, repoID), []int64{10, 20, 40, 30}; !slices.Equal(got, want) {
		t.Fatalf("order after re-rank = %v, want %v", got, want)
	}
}

// A batch led by a priority PR contains only priority PRs, so a broken normal
// PR cannot delay the urgent one through bisection.
func TestFormBatchPriorityOnly(t *testing.T) {
	svc, ctx, repoID := testutil.TestQueueService(t)

	enqueue(t, svc, repoID, false, 10, 20)
	enqueue(t, svc, repoID, true, 30, 40)

	b, err := svc.FormBatch(ctx, repoID, "main", 0)
	if err != nil {
		t.Fatal(err)
	}
	members, err := svc.GetEntriesByIDs(ctx, b.MemberIds)
	if err != nil {
		t.Fatal(err)
	}
	var prs []int64
	for _, m := range members {
		prs = append(prs, m.PrNumber)
	}
	if want := []int64{30, 40}; !slices.Equal(prs, want) {
		t.Fatalf("batch = %v, want %v", prs, want)
	}
}
