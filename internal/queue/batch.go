package queue

import (
	"context"
	"errors"
	"fmt"

	"github.com/Mic92/gitea-mq/internal/store/pg"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// FormBatch atomically takes up to max queued entries for (repo, branch),
// creates a batch row owning them, and marks each entry testing with
// active_batch_id set. Returns nil when there is nothing queued.
// max <= 0 means "everything currently queued".
func (s *Service) FormBatch(ctx context.Context, repoID int64, targetBranch string, max int) (*pg.Batch, error) {
	var batch pg.Batch
	if max <= 0 {
		// Postgres LIMIT NULL is awkward through sqlc; a generous cap is
		// effectively "all".
		max = 1 << 30
	}
	err := s.withTx(ctx, func(q *pg.Queries) error {
		entries, err := q.TakeQueuedHead(ctx, pg.TakeQueuedHeadParams{
			RepoID:       repoID,
			TargetBranch: targetBranch,
			Limit:        int32(max),
		})
		if err != nil {
			return fmt.Errorf("take queued head: %w", err)
		}
		if len(entries) == 0 {
			return nil
		}
		ids := make([]int64, len(entries))
		for i, e := range entries {
			ids[i] = e.ID
		}
		batch, err = q.CreateBatch(ctx, pg.CreateBatchParams{
			RepoID:       repoID,
			TargetBranch: targetBranch,
			MemberIds:    ids,
		})
		if err != nil {
			return fmt.Errorf("create batch: %w", err)
		}
		if err := q.SetEntryActiveBatch(ctx, pg.SetEntryActiveBatchParams{
			ActiveBatchID: pgtype.Int8{Int64: batch.ID, Valid: true},
			Ids:           ids,
		}); err != nil {
			return fmt.Errorf("set active batch: %w", err)
		}
		return nil
	})
	if err != nil || batch.ID == 0 {
		return nil, err
	}
	return &batch, nil
}

// GetLiveBatch returns the live (forming/testing) batch for a queue, or nil.
func (s *Service) GetLiveBatch(ctx context.Context, repoID int64, targetBranch string) (*pg.Batch, error) {
	b, err := s.queries().GetLiveBatch(ctx, pg.GetLiveBatchParams{
		RepoID:       repoID,
		TargetBranch: targetBranch,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &b, nil
}

// GetBatch returns a batch by ID, or nil if not found.
func (s *Service) GetBatch(ctx context.Context, id int64) (*pg.Batch, error) {
	b, err := s.queries().GetBatch(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &b, nil
}

// ListLiveBatches returns all live batches for a repo (across target branches).
func (s *Service) ListLiveBatches(ctx context.Context, repoID int64) ([]pg.Batch, error) {
	return s.queries().ListLiveBatchesByRepo(ctx, repoID)
}

// nn returns s unchanged or an empty non-nil slice; pgx maps a nil Go slice to
// SQL NULL, which the NOT NULL array columns reject.
func nn(s []int64) []int64 {
	if s == nil {
		return []int64{}
	}
	return s
}

// SaveBatch persists the mutable fields of a batch row.
func (s *Service) SaveBatch(ctx context.Context, b *pg.Batch) error {
	pending := b.Pending
	if len(pending) == 0 {
		pending = []byte("[]")
	}
	saved, err := s.queries().SaveBatch(ctx, pg.SaveBatchParams{
		ID:               b.ID,
		State:            b.State,
		CurrentIds:       nn(b.CurrentIds),
		Pending:          pending,
		LandedIds:        nn(b.LandedIds),
		EjectedIds:       nn(b.EjectedIds),
		BranchName:       b.BranchName,
		BranchSha:        b.BranchSha,
		Builds:           b.Builds,
		FfRetries:        b.FfRetries,
		Flaky:            b.Flaky,
		TestingStartedAt: b.TestingStartedAt,
	})
	if err != nil {
		return err
	}
	*b = saved
	return nil
}

// GetEntriesByIDs loads queue entries by primary key, preserving the input
// order so callers can rely on FIFO member ordering.
func (s *Service) GetEntriesByIDs(ctx context.Context, ids []int64) ([]pg.QueueEntry, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.queries().GetEntriesByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]pg.QueueEntry, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}
	out := make([]pg.QueueEntry, 0, len(ids))
	for _, id := range ids {
		if e, ok := byID[id]; ok {
			out = append(out, e)
		}
	}
	return out, nil
}

// ClearMergeBranch nulls merge_branch_{name,sha} for the given entry IDs.
func (s *Service) ClearMergeBranch(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	return s.queries().ClearEntryMergeBranch(ctx, ids)
}

// ClearCheckStatuses deletes recorded check statuses for the given entry IDs.
// Called on each batch rebuild so stale results from a previous build cannot
// satisfy or fail the next one.
func (s *Service) ClearCheckStatuses(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	return s.queries().ClearCheckStatuses(ctx, ids)
}

// CancelLiveBatches marks every live batch for a repo cancelled. Used on repo
// removal so the unique-live index does not block a future re-add.
func (s *Service) CancelLiveBatches(ctx context.Context, repoID int64) error {
	return s.queries().CancelBatchesByRepo(ctx, repoID)
}
