-- name: GetOrCreateRepo :one
INSERT INTO repos (owner, name)
VALUES ($1, $2)
ON CONFLICT (owner, name) DO UPDATE SET owner = EXCLUDED.owner
RETURNING *;

-- name: EnqueuePR :one
INSERT INTO queue_entries (repo_id, pr_number, pr_head_sha, target_branch)
VALUES ($1, $2, $3, $4)
ON CONFLICT (repo_id, pr_number) DO NOTHING
RETURNING *;

-- name: DequeuePR :exec
DELETE FROM queue_entries
WHERE repo_id = $1 AND pr_number = $2;

-- name: ListQueue :many
SELECT * FROM queue_entries
WHERE repo_id = $1 AND target_branch = $2
ORDER BY enqueued_at ASC;

-- name: ListActiveEntriesByRepo :many
SELECT * FROM queue_entries
WHERE repo_id = $1 AND state NOT IN ('failed', 'cancelled')
ORDER BY target_branch, enqueued_at ASC;

-- name: GetQueueEntry :one
SELECT * FROM queue_entries
WHERE repo_id = $1 AND pr_number = $2;

-- name: UpdateEntryState :exec
UPDATE queue_entries
SET state = @state,
    testing_started_at = CASE WHEN @state::entry_state = 'testing' THEN NOW() ELSE testing_started_at END,
    completed_at = CASE WHEN @state::entry_state IN ('success', 'failed', 'cancelled') THEN NOW() ELSE completed_at END
WHERE repo_id = @repo_id AND pr_number = @pr_number;

-- name: UpdateEntryMergeBranch :exec
UPDATE queue_entries
SET merge_branch_name = $3, merge_branch_sha = $4
WHERE repo_id = $1 AND pr_number = $2;

-- name: UpdateEntryError :exec
UPDATE queue_entries
SET error_message = $3
WHERE repo_id = $1 AND pr_number = $2;

-- name: SaveCheckStatus :exec
INSERT INTO check_statuses (queue_entry_id, context, state)
VALUES ($1, $2, $3)
ON CONFLICT (queue_entry_id, context) DO UPDATE
SET state = EXCLUDED.state, updated_at = NOW();

-- name: GetCheckStatuses :many
SELECT * FROM check_statuses
WHERE queue_entry_id = $1;

-- name: LoadActiveQueues :many
SELECT qe.*, r.owner, r.name AS repo_name
FROM queue_entries qe
JOIN repos r ON r.id = qe.repo_id
WHERE qe.state NOT IN ('failed', 'cancelled')
ORDER BY r.owner, r.name, qe.target_branch, qe.enqueued_at ASC;

-- name: GetHeadOfQueue :one
SELECT * FROM queue_entries
WHERE repo_id = $1 AND target_branch = $2
ORDER BY enqueued_at ASC
LIMIT 1;

-- name: CountQueuePosition :one
SELECT COUNT(*) FROM queue_entries qe
WHERE qe.repo_id = $1 AND qe.target_branch = $2
  AND qe.enqueued_at <= (SELECT qe2.enqueued_at FROM queue_entries qe2 WHERE qe2.repo_id = $1 AND qe2.pr_number = $3);


