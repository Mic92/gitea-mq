package queue_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mic92/gitea-mq/internal/queue"
	"github.com/Mic92/gitea-mq/internal/store/pg"
	"github.com/Mic92/gitea-mq/internal/testutil"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// jitter delays each statement to shuffle concurrent interleavings.
type jitter struct{ max time.Duration }

func (j jitter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	time.Sleep(rand.N(j.max + 1))
	return ctx
}

func (jitter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func jitterPool(t *testing.T, conns int32, max time.Duration, params map[string]string) *pgxpool.Pool {
	t.Helper()
	cfg := testutil.TestDB(t).Config().Copy()
	cfg.MaxConns = conns
	cfg.ConnConfig.Tracer = jitter{max: max}
	for k, v := range params {
		cfg.ConnConfig.RuntimeParams[k] = v
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

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

func TestConcurrentFormBatchOnSeparateBranches(t *testing.T) {
	const workers, perBranch = 16, 3
	svc := queue.NewService(jitterPool(t, workers, 2*time.Millisecond, nil))
	ctx := t.Context()
	repo, err := svc.GetOrCreateRepo(ctx, "gitea", "org", "app")
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := range workers {
		wg.Go(func() {
			branch := fmt.Sprintf("b%d", w)
			for i := range perBranch {
				if _, err := svc.Enqueue(ctx, repo.ID, int64(w*perBranch+i), "sha", branch, false); err != nil {
					errs <- err
					return
				}
			}
			b, err := svc.FormBatch(ctx, repo.ID, branch, 0)
			if err == nil && len(b.MemberIds) != perBranch {
				err = fmt.Errorf("batch for %s has %d members, want %d", branch, len(b.MemberIds), perBranch)
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
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
