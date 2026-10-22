-- name: GetOrCreateRepo :one
INSERT INTO repos (forge, owner, name)
VALUES ($1, $2, $3)
ON CONFLICT (forge, owner, name) DO UPDATE SET owner = EXCLUDED.owner
RETURNING *;

-- name: EnqueuePR :one
INSERT INTO queue_entries (repo_id, pr_number, pr_head_sha, target_branch, priority)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (repo_id, pr_number) DO NOTHING
RETURNING *;

-- name: DequeuePR :exec
DELETE FROM queue_entries
WHERE repo_id = $1 AND pr_number = $2;

-- Queue order everywhere: in-flight entries first so a priority arrival
-- never preempts them, then priority, then FIFO.

-- name: ListActiveEntriesByRepo :many
SELECT * FROM queue_entries
WHERE repo_id = $1
ORDER BY target_branch, state = 'queued', priority DESC, enqueued_at, id;

-- name: GetQueueEntry :one
SELECT * FROM queue_entries
WHERE repo_id = $1 AND pr_number = $2;

-- name: UpdateEntryState :exec
UPDATE queue_entries
SET state = @state,
    testing_started_at = CASE WHEN @state::entry_state = 'testing' THEN NOW() ELSE testing_started_at END,
    completed_at = CASE WHEN @state::entry_state = 'success' THEN NOW() ELSE completed_at END
WHERE repo_id = @repo_id AND pr_number = @pr_number;

-- name: UpdateEntryMergeBranch :exec
UPDATE queue_entries
SET merge_branch_name = $3, merge_branch_sha = $4
WHERE repo_id = $1 AND pr_number = $2;

-- name: SetEntryPriority :exec
UPDATE queue_entries SET priority = $3
WHERE repo_id = $1 AND pr_number = $2 AND state = 'queued';

-- name: UpdateEntryError :exec
UPDATE queue_entries
SET error_message = $3
WHERE repo_id = $1 AND pr_number = $2;

-- name: SaveCheckStatus :exec
INSERT INTO check_statuses (queue_entry_id, context, state, target_url)
VALUES ($1, $2, $3, $4)
ON CONFLICT (queue_entry_id, context) DO UPDATE
SET state = EXCLUDED.state, target_url = EXCLUDED.target_url, updated_at = NOW();

-- name: GetCheckStatuses :many
SELECT * FROM check_statuses
WHERE queue_entry_id = $1;

-- name: GetHeadOfQueue :one
SELECT * FROM queue_entries
WHERE repo_id = $1 AND target_branch = $2
ORDER BY state = 'queued', priority DESC, enqueued_at, id
LIMIT 1;

-- name: CountQueuePosition :one
SELECT COUNT(*) FROM queue_entries qe
JOIN queue_entries me ON me.repo_id = qe.repo_id AND me.pr_number = $3
WHERE qe.repo_id = $1 AND qe.target_branch = $2
  AND (qe.state = 'queued', NOT qe.priority, qe.enqueued_at, qe.id)
   <= (me.state = 'queued', NOT me.priority, me.enqueued_at, me.id);

-- name: DequeueAllByRepo :exec
DELETE FROM queue_entries
WHERE repo_id = $1;

-- name: TakeQueuedHead :many
SELECT q.* FROM queue_entries q
WHERE q.repo_id = $1 AND q.target_branch = $2 AND q.state = 'queued'
  -- Priority batches exclude normal PRs so one cannot delay the other via bisection.
  AND (q.priority OR NOT EXISTS (
    SELECT 1 FROM queue_entries p
    WHERE p.repo_id = $1 AND p.target_branch = $2 AND p.state = 'queued' AND p.priority))
ORDER BY q.enqueued_at, q.id
LIMIT $3;

-- name: GetEntriesByIDs :many
SELECT * FROM queue_entries
WHERE id = ANY(@ids::bigint[]);

-- name: DeleteEntriesByIDs :exec
DELETE FROM queue_entries
WHERE id = ANY(@ids::bigint[]);

-- name: SetEntryActiveBatch :exec
UPDATE queue_entries
SET active_batch_id = @active_batch_id, state = 'testing',
    testing_started_at = COALESCE(testing_started_at, NOW())
WHERE id = ANY(@ids::bigint[]);

-- name: ClearEntryMergeBranch :exec
UPDATE queue_entries
SET merge_branch_name = NULL, merge_branch_sha = NULL
WHERE id = ANY(@ids::bigint[]);

-- name: ClearCheckStatuses :exec
DELETE FROM check_statuses
WHERE queue_entry_id = ANY(@ids::bigint[]);

-- name: CreateBatch :one
INSERT INTO batches (repo_id, target_branch, member_ids, current_ids)
VALUES ($1, $2, $3, $3)
RETURNING *;

-- name: GetBatch :one
SELECT * FROM batches WHERE id = $1;

-- name: GetLiveBatch :one
SELECT * FROM batches
WHERE repo_id = $1 AND target_branch = $2 AND state IN ('forming', 'testing');

-- name: ListLiveBatchesByRepo :many
SELECT * FROM batches
WHERE repo_id = $1 AND state IN ('forming', 'testing');

-- name: SaveBatch :one
UPDATE batches SET
    state = $2,
    current_ids = $3,
    pending = $4,
    landed_ids = $5,
    ejected_ids = $6,
    branch_name = $7,
    branch_sha = $8,
    builds = $9,
    ff_retries = $10,
    flaky = $11,
    testing_started_at = $12
WHERE id = $1 AND state IN ('forming', 'testing')
RETURNING *;

-- name: CancelBatchesByRepo :exec
UPDATE batches SET state = 'cancelled'
WHERE repo_id = $1 AND state IN ('forming', 'testing');
