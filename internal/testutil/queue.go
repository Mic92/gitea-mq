package testutil

import (
	"testing"

	"github.com/Mic92/gitea-mq/internal/merge"
	"github.com/Mic92/gitea-mq/internal/queue"
	"github.com/Mic92/gitea-mq/internal/store/pg"
)

// EnqueueTesting puts a PR into the testing state with a merge branch set,
// the precondition for the monitor and webhook handlers to act on it.
func EnqueueTesting(t *testing.T, svc *queue.Service, repoID, pr int64, headSHA, mergeSHA string) *pg.QueueEntry {
	t.Helper()
	ctx := t.Context()

	if _, err := svc.Enqueue(ctx, repoID, pr, headSHA, "main"); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateState(ctx, repoID, pr, pg.EntryStateTesting); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetMergeBranch(ctx, repoID, pr, merge.BranchName(pr), mergeSHA); err != nil {
		t.Fatal(err)
	}
	entry, err := svc.GetEntry(ctx, repoID, pr)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}
