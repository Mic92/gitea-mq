-- +goose Up

CREATE TABLE repos (
    id         BIGSERIAL PRIMARY KEY,
    owner      TEXT NOT NULL,
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (owner, name)
);

CREATE TYPE entry_state AS ENUM (
    'queued',
    'testing',
    'success',
    'failed',
    'cancelled'
);

CREATE TABLE queue_entries (
    id                  BIGSERIAL PRIMARY KEY,
    repo_id             BIGINT NOT NULL REFERENCES repos(id),
    pr_number           BIGINT NOT NULL,
    pr_head_sha         TEXT NOT NULL,
    target_branch       TEXT NOT NULL,
    state               entry_state NOT NULL DEFAULT 'queued',
    enqueued_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    testing_started_at  TIMESTAMPTZ,
    completed_at        TIMESTAMPTZ,
    merge_branch_name   TEXT,
    merge_branch_sha    TEXT,
    error_message       TEXT,
    UNIQUE (repo_id, pr_number)
);

CREATE INDEX idx_queue_entries_repo_state ON queue_entries(repo_id, state);
CREATE INDEX idx_queue_entries_repo_branch_order ON queue_entries(repo_id, target_branch, enqueued_at);

CREATE TYPE check_state AS ENUM (
    'pending',
    'success',
    'failure',
    'error'
);

CREATE TABLE check_statuses (
    id              BIGSERIAL PRIMARY KEY,
    queue_entry_id  BIGINT NOT NULL REFERENCES queue_entries(id) ON DELETE CASCADE,
    context         TEXT NOT NULL,
    state           check_state NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (queue_entry_id, context)
);

-- +goose Down

DROP TABLE IF EXISTS check_statuses;
DROP TABLE IF EXISTS queue_entries;
DROP TYPE IF EXISTS check_state;
DROP TYPE IF EXISTS entry_state;
DROP TABLE IF EXISTS repos;
