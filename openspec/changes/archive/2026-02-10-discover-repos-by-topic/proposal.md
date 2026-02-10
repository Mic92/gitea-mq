## Why

Operators currently hardcode a list of repos via `GITEA_MQ_REPOS`. This doesn't scale — adding or removing a repo means editing config and restarting the service. buildbot-nix solved this by discovering repos via a Gitea topic (e.g., `merge-queue`): any repo with that topic and sufficient access is automatically picked up. gitea-mq should do the same so that onboarding a new repo is just a tag away.

## What Changes

- **New: topic-based repo discovery** — On startup and periodically, query the Gitea API for all repos the authenticated user can access, fetch each repo's topics, and filter to those matching a configured topic string. The discovered repo set dynamically replaces the hardcoded list.
- **BREAKING: `GITEA_MQ_REPOS` becomes optional** — When a topic is configured (`GITEA_MQ_TOPIC`), the explicit repo list is no longer required. If both are set, the topic-discovered repos are merged with the explicit list (explicit repos always included). If neither is set, startup fails.
- **Dynamic repo set** — The poller, webhook router, web dashboard, and auto-setup all operate on a repo set that can change at runtime as repos gain/lose the topic. New repos are initialised (DB registration, branch protection, webhook setup); removed repos are gracefully drained.
- **NixOS module gains `topic` option** — `services.gitea-mq.topic` replaces or supplements `repos`. When `topic` is set, `repos` defaults to `[]` and is no longer required.

## Capabilities

### New Capabilities
- `repo-discovery`: Periodic discovery of repos by Gitea topic, including access verification, topic fetching, and dynamic reconciliation of the managed repo set.

### Modified Capabilities
- `automerge-integration`: Discovery loop must register newly-discovered repos with the poller so automerge detection starts for them.
- `webhook-receiver`: Webhook router must accept events from dynamically-discovered repos (not just the initial static set).
- `web-dashboard`: Dashboard must reflect the current dynamic repo set, not a fixed list from config.

## Impact

- **Config** (`internal/config/config.go`): New `Topic` field; `Repos` becomes optional when `Topic` is set.
- **Gitea client** (`internal/gitea/`): Two new API methods — `ListUserRepos` (paginated `GET /api/v1/user/repos`) and `GetRepoTopics` (`GET /api/v1/repos/{owner}/{name}/topics`). Note: Gitea does not include topics in the default repo listing, so a per-repo call is needed (matching buildbot-nix's approach).
- **Main entrypoint** (`cmd/gitea-mq/main.go`): Startup must run initial discovery; a background goroutine re-discovers periodically.
- **Webhook routing** (`internal/webhook/`): `repoMonitors` map must support concurrent add/remove.
- **Web dashboard** (`internal/web/`): `ManagedRepos` must be read dynamically rather than captured at init.
- **NixOS module** (`nix/module.nix`): New `topic` option; `repos` assertion relaxed; new `GITEA_MQ_TOPIC` env var.
- **Integration tests** (`nix/test.nix`): Need a test that sets a topic on a repo and verifies discovery.
