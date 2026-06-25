-- +goose Up
CREATE TYPE batch_state AS ENUM ('forming', 'testing', 'done', 'cancelled');

CREATE TABLE batches (
    id                  BIGSERIAL PRIMARY KEY,
    repo_id             BIGINT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    target_branch       TEXT   NOT NULL,
    state               batch_state NOT NULL DEFAULT 'forming',

    member_ids          BIGINT[] NOT NULL,
    current_ids         BIGINT[] NOT NULL,
    pending             JSONB    NOT NULL DEFAULT '[]',
    landed_ids          BIGINT[] NOT NULL DEFAULT '{}',
    ejected_ids         BIGINT[] NOT NULL DEFAULT '{}',

    branch_name         TEXT,
    branch_sha          TEXT,
    builds              INT NOT NULL DEFAULT 0,
    ff_retries          INT NOT NULL DEFAULT 0,
    flaky               BOOLEAN NOT NULL DEFAULT FALSE,

    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    testing_started_at  TIMESTAMPTZ
);

CREATE UNIQUE INDEX ux_batches_live ON batches(repo_id, target_branch)
    WHERE state IN ('forming', 'testing');

ALTER TABLE queue_entries
    ADD COLUMN active_batch_id BIGINT REFERENCES batches(id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE queue_entries DROP COLUMN active_batch_id;
DROP INDEX IF EXISTS ux_batches_live;
DROP TABLE IF EXISTS batches;
DROP TYPE IF EXISTS batch_state;
