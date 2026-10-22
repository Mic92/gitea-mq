-- +goose Up
ALTER TABLE queue_entries ADD COLUMN priority BOOLEAN NOT NULL DEFAULT FALSE;
-- Failed entries are now dequeued directly; drop leftovers so queries need
-- not filter terminal states.
DELETE FROM queue_entries WHERE state IN ('failed', 'cancelled');

-- +goose Down
ALTER TABLE queue_entries DROP COLUMN priority;
