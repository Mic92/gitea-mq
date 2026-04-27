-- +goose Up
ALTER TABLE repos ADD COLUMN forge TEXT NOT NULL DEFAULT 'gitea';
ALTER TABLE repos DROP CONSTRAINT repos_owner_name_key;
ALTER TABLE repos ADD CONSTRAINT repos_forge_owner_name_key UNIQUE (forge, owner, name);

-- +goose Down
ALTER TABLE repos DROP CONSTRAINT repos_forge_owner_name_key;
ALTER TABLE repos ADD CONSTRAINT repos_owner_name_key UNIQUE (owner, name);
ALTER TABLE repos DROP COLUMN forge;
