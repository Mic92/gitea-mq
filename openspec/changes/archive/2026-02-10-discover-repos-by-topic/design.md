## Context

gitea-mq currently requires an explicit list of repos (`GITEA_MQ_REPOS`). Each repo is initialised at startup: DB registration, branch protection setup, webhook creation, poller goroutine, and webhook router entry. Adding or removing a repo requires editing config and restarting the service.

buildbot-nix solved this with topic-based discovery: it calls `GET /api/v1/user/repos` (paginated), fetches topics per repo via `GET /api/v1/repos/{owner}/{name}/topics` (Gitea doesn't include topics in the listing endpoint), filters by a configured topic string, and only picks up repos where the token owner has admin access.

The key architectural challenge is that gitea-mq's current design assumes a static repo set — the `repoMonitors` map, `ManagedRepos` slice, and poller goroutines are all created once at startup and never change. Making the repo set dynamic requires a coordination layer.

## Goals / Non-Goals

**Goals:**
- Discover repos by a Gitea topic, filtering to repos the authenticated user has access to
- Support both topic-only, static-only, and combined (topic + explicit) modes
- Add/remove repos at runtime without restart — including poller goroutines, webhook routing, and dashboard
- Reuse the existing per-repo setup logic (branch protection, webhook, DB registration) for dynamically discovered repos

**Non-Goals:**
- Multi-topic support (single topic string is sufficient for now)
- Watching for topic changes in real-time via webhooks (periodic polling is fine)
- Removing DB data for repos that lose the topic (we just stop monitoring them)
- Org-level filtering or user allowlists (buildbot-nix has these; we don't need them yet)

## Decisions

### 1. New `RepoRegistry` coordination type

**Decision**: Introduce an `internal/registry` package with a `RepoRegistry` struct that owns the lifecycle of per-repo resources (poller goroutines, monitor deps, webhook routing entries).

**Rationale**: Currently `main.go` manages per-repo resources inline in a loop. This can't support dynamic add/remove. A registry centralises the "what repos are we managing" question and provides thread-safe `Add(ref)` / `Remove(ref)` / `List()` / `Lookup(fullName)` methods.

**Alternatives considered**:
- *Modify the existing maps in-place with mutexes*: Scatters concurrency logic across webhook handler, web handler, and main. Registry is cleaner.
- *Channel-based actor*: More complex than needed — a `sync.RWMutex` over a map is sufficient given the low churn rate (discovery runs every few minutes).

### 2. Discovery as a periodic background loop

**Decision**: A new `internal/discovery` package runs a periodic loop (reusing the existing `PollInterval` or a new `DiscoveryInterval` config). Each cycle calls the Gitea API, computes the desired repo set (topic-discovered ∪ explicit), diffs against the current registry, and calls `registry.Add` / `registry.Remove`.

**Rationale**: Matches the polling pattern already used for automerge detection. No need for a separate webhook — Gitea doesn't send webhooks when topics change.

**Interval**: Default to 5 minutes. Topic changes are infrequent; no need to poll as aggressively as automerge detection (30s).

### 3. Gitea API: `ListUserRepos` + per-repo `GetRepoTopics`

**Decision**: Two new methods on the `gitea.Client` interface:
- `ListUserRepos(ctx) ([]Repo, error)` — paginated `GET /api/v1/user/repos?limit=50`
- `GetRepoTopics(ctx, owner, repo) ([]string, error)` — `GET /api/v1/repos/{owner}/{name}/topics`

**Rationale**: Gitea's repo listing endpoint does not include topics (unlike GitHub). buildbot-nix works around this the same way. We also need `ListUserRepos` to return permission info so we can filter to repos with admin access (needed for webhook/branch protection setup).

A new `Repo` struct (distinct from `PR`) holds the minimal fields: `FullName`, `Owner.Login`, `Name`, `Permissions.Admin`.

### 4. Access check: require admin permission

**Decision**: Only discover repos where the token owner has admin permission (`permissions.admin == true` in the API response).

**Rationale**: gitea-mq needs to create webhooks and edit branch protection, both of which require admin access. Discovering a repo without admin would cause setup failures. buildbot-nix does the same check.

### 5. Config: `GITEA_MQ_TOPIC` env var, `Repos` becomes conditionally optional

**Decision**: Add `GITEA_MQ_TOPIC` (string). Validation:
- If `GITEA_MQ_TOPIC` is set: `GITEA_MQ_REPOS` is optional (defaults to empty)
- If `GITEA_MQ_TOPIC` is not set: `GITEA_MQ_REPOS` is required (current behaviour)
- If both are set: explicit repos are always included, topic-discovered repos are merged in

**Rationale**: Backwards-compatible. Existing deployments with only `GITEA_MQ_REPOS` continue working unchanged.

### 6. Webhook handler and web dashboard read from registry

**Decision**: The webhook `Handler` function and web `Deps` receive a pointer/interface to the registry instead of a static map/slice. Webhook handler calls `registry.Lookup(fullName)` per request. Dashboard calls `registry.List()` per page render.

**Rationale**: Both are already per-request (no caching of the repo list across requests), so reading from the registry on each request is natural and adds no meaningful overhead.

### 7. Graceful removal: stop poller, keep DB data

**Decision**: When a repo loses the topic, the registry cancels its poller goroutine's context and removes it from the webhook router. The DB data (queue entries, repos table row) is left intact.

**Rationale**: The repo might get the topic back. Preserving DB data avoids re-discovering in-flight PRs. Stale data is harmless — queue entries have TTLs via the existing timeout mechanism.

### 8. Discovery interval as a separate config

**Decision**: Add `GITEA_MQ_DISCOVERY_INTERVAL` (default `5m`). Separate from `GITEA_MQ_POLL_INTERVAL`.

**Rationale**: Discovery is API-heavy (one call per repo for topics) and topic changes are infrequent. Tying it to the 30s automerge poll interval would waste API calls.

## Risks / Trade-offs

- **API rate limiting** — Discovery calls `ListUserRepos` (1 paginated call) + `GetRepoTopics` (1 call per repo). For a Gitea instance with 100 repos, that's ~3 calls every 5 minutes. Low risk, but could be a concern for very large instances. → Mitigation: configurable interval; could add topic caching later.

- **Race between discovery and webhook events** — A webhook event might arrive for a repo that's mid-setup (just discovered but not yet fully initialised). → Mitigation: The registry's `Add` method completes setup before making the repo visible to the webhook handler. The webhook handler silently ignores unknown repos (existing behaviour).

- **Poller goroutine leak** — If `Remove` doesn't properly cancel the poller context, goroutines accumulate. → Mitigation: Each poller goroutine gets a per-repo `context.WithCancel`; registry stores the cancel func and calls it on remove. Test with goroutine counting.

- **First-run latency** — Initial discovery must complete before the service starts serving. If the Gitea API is slow or has many repos, startup is delayed. → Mitigation: Run initial discovery with a timeout; fall back to explicit repos if discovery fails.

## Migration Plan

- **Backwards-compatible**: No config changes required for existing deployments. `GITEA_MQ_REPOS` continues to work as before.
- **Opt-in**: Set `GITEA_MQ_TOPIC=merge-queue` (or whatever topic) to enable discovery.
- **NixOS module**: Add `topic` option. When set, `repos` defaults to `[]` and is no longer required. Existing configs with only `repos` continue working.
- **Rollback**: Remove `GITEA_MQ_TOPIC` from config → back to static behaviour.

## Open Questions

- Should the discovery loop also handle webhook auto-setup for newly discovered repos, or should that be opt-in per topic? (Current design: yes, auto-setup runs for all discovered repos, same as for explicit repos.)
- Should we log which repos were added/removed on each discovery cycle? (Likely yes, at info level.)
