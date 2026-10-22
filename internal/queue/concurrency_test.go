package queue_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Mic92/gitea-mq/internal/queue"
	"github.com/Mic92/gitea-mq/internal/store/pg"
	"github.com/Mic92/gitea-mq/internal/testutil"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestEnqueueSurfacesInsertError(t *testing.T) {
	svc, ctx, _ := testutil.TestQueueService(t)

	_, err := svc.Enqueue(ctx, 999999, 1, "sha", "main", false)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("want foreign key violation, got %v", err)
	}
	if strings.Contains(err.Error(), "25P02") {
		t.Fatalf("error masked by lookup on aborted transaction: %v", err)
	}
}

// Repo removal cancels batches outside the engine lock.
func TestSaveBatchDoesNotReviveCancelled(t *testing.T) {
	svc, ctx, repoID := testutil.TestQueueService(t)
	if _, err := svc.Enqueue(ctx, repoID, 1, "sha", "main", false); err != nil {
		t.Fatal(err)
	}
	b, err := svc.FormBatch(ctx, repoID, "main", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.CancelLiveBatches(ctx, repoID); err != nil {
		t.Fatal(err)
	}

	b.State = pg.BatchStateTesting
	if err := svc.SaveBatch(ctx, b, b.MemberIds...); !errors.Is(err, queue.ErrBatchNotLive) {
		t.Fatalf("SaveBatch on cancelled batch: err = %v, want ErrBatchNotLive", err)
	}
	if got, _ := svc.GetBatch(ctx, b.ID); got.State != pg.BatchStateCancelled {
		t.Fatalf("batch state = %s, want cancelled", got.State)
	}
	if e, _ := svc.GetEntry(ctx, repoID, 1); e == nil {
		t.Fatal("entry removal must roll back with the rejected save")
	}
}
