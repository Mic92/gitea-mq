-- +goose Up
ALTER TABLE check_statuses ADD COLUMN target_url TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE check_statuses DROP COLUMN target_url;
