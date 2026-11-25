-- +goose Up
ALTER TABLE queue_entries ADD COLUMN pr_author TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE queue_entries DROP COLUMN pr_author;
