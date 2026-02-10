## 1. Gitea Client: New API Methods

- [x] 1.1 Add `Repo` struct to `internal/gitea/client.go` with fields: `FullName`, `Owner` (struct with `Login`), `Name`, `Permissions` (struct with `Admin bool`), matching Gitea API JSON
- [x] 1.2 Add `ListUserRepos(ctx) ([]Repo, error)` to `Client` interface — paginated `GET /api/v1/user/repos?limit=50`
- [x] 1.3 Add `GetRepoTopics(ctx, owner, repo) ([]string, error)` to `Client` interface — `GET /api/v1/repos/{owner}/{name}/topics`
- [x] 1.4 Implement `ListUserRepos` in `HTTPClient` with pagination (follow `page` query param until result count < limit)
- [x] 1.5 Implement `GetRepoTopics` in `HTTPClient`, parsing the `{"topics": [...]}` response
- [x] 1.6 Add unit tests for `ListUserRepos` and `GetRepoTopics` using httptest server (test pagination, empty results, error responses)

## 2. Config: Topic and Discovery Interval

- [x] 2.1 Add `Topic string` field to `Config` struct, read from `GITEA_MQ_TOPIC` env var
- [x] 2.2 Add `DiscoveryInterval time.Duration` field, read from `GITEA_MQ_DISCOVERY_INTERVAL` with default `5m`
- [x] 2.3 Change `Repos`/`GITEA_MQ_REPOS` validation: required only when `Topic` is empty; when `Topic` is set, `Repos` defaults to empty
- [x] 2.4 Add validation: at least one of `Topic` or `Repos` must be set, fail with clear error otherwise
- [x] 2.5 Add unit tests for config loading: topic-only, repos-only, combined, neither (error)

## 3. Registry: Repo Lifecycle Coordination

- [x] 3.1 Create `internal/registry/` package with `RepoRegistry` struct holding a `sync.RWMutex`-protected map of `fullName → *ManagedRepo`
- [x] 3.2 Define `ManagedRepo` struct: `RepoRef`, DB `RepoID`, `*monitor.Deps`, `cancelFunc context.CancelFunc` (for stopping poller)
- [x] 3.3 Implement `Add(ctx, ref) error`: register repo in DB, run auto-setup (branch protection + webhook), create per-repo context, start poller goroutine, insert into map — repo becomes visible to Lookup/List only after setup completes
- [x] 3.4 Implement `Remove(ref)`: cancel per-repo context (stops poller), delete from map, log removal. Do NOT touch DB data
- [x] 3.5 Implement `Lookup(fullName) (*ManagedRepo, bool)`: read-locked map lookup, used by webhook handler
- [x] 3.6 Implement `List() []config.RepoRef`: read-locked snapshot of current managed repos, used by web dashboard
- [x] 3.7 Implement `Contains(fullName) bool`: convenience for web dashboard repo detail page access check
- [x] 3.8 Add unit tests for registry: concurrent Add/Remove/Lookup/List, Add idempotency, Remove of non-existent repo

## 4. Discovery Loop

- [x] 4.1 Create `internal/discovery/` package with `Deps` struct: `gitea.Client`, `*registry.RepoRegistry`, `Topic string`, `ExplicitRepos []config.RepoRef`
- [x] 4.2 Implement `DiscoverOnce(ctx, deps) error`: call `ListUserRepos`, filter by admin permission, fetch topics per repo, filter by topic, merge with explicit repos, diff against registry, call `Add`/`Remove`
- [x] 4.3 Implement partial failure handling: if `GetRepoTopics` fails for one repo, skip it but don't remove previously-managed repos (conservative reconciliation)
- [x] 4.4 Implement `Run(ctx, deps, interval)`: immediate first run, then ticker loop (same pattern as poller)
- [x] 4.5 Add unit tests for `DiscoverOnce`: topic matching, admin filtering, merge with explicit repos, add/remove reconciliation, API failure handling, partial topic fetch failure

## 5. Refactor main.go: Use Registry

- [x] 5.1 Extract per-repo setup from `main.go`'s inline loop into a function that `registry.Add` can call (DB registration, branch protection, webhook, poller start)
- [x] 5.2 Create `RepoRegistry` in `run()` and initialise it with explicit repos (replaces the current `repoMonitors` map construction)
- [x] 5.3 Pass registry to webhook `Handler` instead of `map[string]*RepoMonitor` — handler calls `registry.Lookup(fullName)` per request
- [x] 5.4 Pass registry to web `Deps` instead of `[]config.RepoRef` — dashboard calls `registry.List()` and `registry.Contains()` per request

## 6. Refactor Webhook Handler

- [x] 6.1 Change `Handler` signature: accept a `RepoLookup` interface (with `Lookup(fullName) (*RepoMonitor, bool)`) instead of `map[string]*RepoMonitor`
- [x] 6.2 Update handler to call the interface method instead of direct map access
- [x] 6.3 Update webhook tests to provide a mock `RepoLookup` (or use registry directly with test setup)

## 7. Refactor Web Dashboard

- [x] 7.1 Change `Deps` to accept a `RepoLister` interface (with `List() []config.RepoRef` and `Contains(fullName) bool`) instead of `ManagedRepos []config.RepoRef`
- [x] 7.2 Update `overviewHandler` to call `registry.List()` on each request instead of reading a static slice
- [x] 7.3 Update `repoHandler` to call `registry.Contains()` for access check instead of iterating a static slice
- [x] 7.4 Update web tests to provide a mock `RepoLister` or use registry with test setup

## 8. Wire Discovery Loop in main.go

- [x] 8.1 If `cfg.Topic` is set, run initial `DiscoverOnce` before starting HTTP server (blocks until complete)
- [x] 8.2 Start discovery `Run` goroutine with `cfg.DiscoveryInterval` (only when `cfg.Topic` is set)
- [x] 8.3 If `cfg.Topic` is not set, skip discovery entirely (static-only mode, existing behaviour preserved)

## 9. NixOS Module

- [x] 9.1 Add `topic` option (`lib.types.nullOr lib.types.str`, default `null`) to `nix/module.nix`
- [x] 9.2 Add `discoveryInterval` option (`lib.types.str`, default `"5m"`) to `nix/module.nix`
- [x] 9.3 Change `repos` option: default to `[]`, add assertion that at least one of `topic` or `repos` is set
- [x] 9.4 Pass `GITEA_MQ_TOPIC` and `GITEA_MQ_DISCOVERY_INTERVAL` env vars in systemd environment (conditionally, only when set)
- [x] 9.5 Make `GITEA_MQ_REPOS` conditional: only set when `repos` is non-empty

## 10. Integration Test

- [x] 10.1 Add NixOS integration test in `nix/test.nix`: configure a Gitea instance with a repo that has a topic, start gitea-mq with `GITEA_MQ_TOPIC`, verify the repo is discovered and a poller is running
- [x] 10.2 Test dynamic add: add the topic to a second repo via Gitea API, wait for discovery interval, verify the repo appears in the dashboard
- [ ] 10.3 Test dynamic remove: remove the topic from a repo, wait for discovery interval, verify the repo disappears from the dashboard
