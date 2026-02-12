## Context

The dashboard currently has two levels: an overview page (`/`) showing all repos with queue metadata, and a repo detail page (`/repo/{owner}/{name}`) showing queued PRs and check statuses for the head-of-queue PR. This change splits the UI into three focused levels and adds clickable links from Gitea commit statuses into the dashboard.

The codebase is a single Go binary. The web layer lives in `internal/web/` with embedded HTML templates and a handler file. Commit statuses are posted from three packages (`poller`, `merge`, `monitor`) via `gitea.MQStatus()`. Config is loaded from environment variables in `internal/config/`.

## Goals / Non-Goals

**Goals:**
- Three-level navigation: projects → PRs → checks
- Each page does one thing well — no overloaded views
- Clickable `target_url` in Gitea commit statuses linking to the PR dashboard page
- Breadcrumb navigation on all pages
- Graceful degradation when Gitea API is unavailable (PR detail page renders without title/author)

**Non-Goals:**
- No database schema changes — PR title/author fetched from Gitea API at render time
- No JavaScript framework — stays server-rendered with `<meta>` auto-refresh
- No authentication changes — dashboard remains public and read-only
- No historical/completed PR views — repo page shows active entries only (queued + testing)

## Decisions

### 1. PR detail page fetches title/author from Gitea API per request

**Decision**: The PR detail page calls `GetPR()` on each page load to get title and author. If the API call fails, the page renders with placeholders ("—") instead of erroring.

**Alternatives considered**:
- *Store title/author in DB at enqueue time*: Adds a schema migration, but titles could go stale if updated on the Gitea side. Rejected to keep the DB schema unchanged.
- *Skip title/author entirely*: Less useful — users would need to cross-reference with Gitea. Rejected because the API call is cheap (one GET per page view).

**Rationale**: The PR detail page is a single-PR view, so one API call per load is acceptable. The `GetPR` method already exists on the `gitea.Client` interface and is used by the poller. Auto-refresh at the default 10s interval means ~6 API calls/minute per open browser tab — negligible for a Gitea instance.

### 2. ExternalURL threaded through Deps structs + DashboardPRURL helper

**Decision**: Add `ExternalURL string` to `registry.Deps`, `monitor.Deps`, and `poller.Deps`. Add a `gitea.DashboardPRURL(baseURL, owner, repo, prNumber) string` helper that centralizes the URL format. Change `MQStatus` signature to `MQStatus(state, description, targetURL string)`.

**How it flows**:
- `config.Load()` reads `GITEA_MQ_EXTERNAL_URL` (now required)
- `main.go` passes it to `registry.Deps{ExternalURL: cfg.ExternalURL}`
- `registry.Add()` propagates to `monitor.Deps` and `poller.Deps`
- Each `MQStatus` call site builds the URL: `gitea.DashboardPRURL(deps.ExternalURL, owner, repo, entry.PrNumber)`
- `merge.StartTesting()` receives `externalURL` as an additional parameter (it takes individual args, not a Deps struct)

**Call sites** (7 total):
- `poller.go`: enqueue pending status, automerge timeout error — 2 sites
- `merge.go`: conflict failure, branch error, testing pending — 3 sites
- `monitor.go`: success, removeFromQueue (failure/timeout) — 2 sites

**Alternatives considered**:
- *Keep MQStatus signature, set TargetURL on returned struct*: Scatters URL construction across 7 sites without a central pattern. Rejected for maintainability.
- *Pass a URL builder function instead of a string*: Over-engineered for a simple `fmt.Sprintf`. Rejected.

### 3. ExternalURL becomes required (BREAKING)

**Decision**: `config.Load()` adds `GITEA_MQ_EXTERNAL_URL` to the required variables list. The service fails to start if it's unset.

**Rationale**: The URL is needed both for webhook auto-setup (existing) and commit status links (new). Without it, the dashboard links feature doesn't work. A clean break is simpler than degraded-mode logic.

**Migration**: Users who previously omitted it (relying on manual webhook setup) must now set it. The error message from `config.Load()` is clear about what's missing.

### 4. Web handler gets a Gitea client for API calls

**Decision**: Add a `gitea.Client` to `web.Deps` so the PR detail handler can call `GetPR()`. Currently `web.Deps` only has `Queue` and `Repos`.

**Rationale**: The web package has been pure DB reads until now. Adding the Gitea client for one endpoint (PR detail) is a minimal change. The handler uses it read-only (single GET call), so there's no risk of the dashboard mutating state.

### 5. Route structure for the PR detail page

**Decision**: `GET /repo/{owner}/{name}/pr/{number}` served by a new `prHandler` function registered on the web mux.

**Parsing**: The existing `repoHandler` already parses `/repo/{owner}/{name}` by trimming the prefix and splitting on `/`. The new handler parses further: after `{name}`, expect `/pr/{number}`. If the format doesn't match, return 404.

**Registration**: In `NewMux`, add `mux.HandleFunc("/repo/", ...)` — the existing `/repo/` pattern stays. The handler checks for the `/pr/` segment to decide between repo view and PR view. Alternatively, register the PR handler separately, but Go's `http.ServeMux` (pre-1.22) doesn't support path parameters, so both `/repo/org/app` and `/repo/org/app/pr/42` match `/repo/`. A single handler that dispatches based on path segments is cleaner.

**Implementation**: Refactor `repoHandler` to detect the presence of `/pr/{number}` after `{name}` and dispatch to either repo-list rendering or PR-detail rendering internally.

### 6. Breadcrumb navigation

**Decision**: All pages include a breadcrumb bar at the top. The breadcrumb is built from the page context:

- Overview: `gitea-mq` (no link, current page)
- Repo: `gitea-mq` › `owner/repo` 
- PR: `gitea-mq` › `owner/repo` › `PR #42`

The current page segment is plain text (not linked). Parent segments are links. Rendered as a `<nav>` element with a `breadcrumb` class. Replaces the existing `<p class="back">` link on the repo page.

### 7. Overview page keeps queue count badge

**Decision**: The overview page shows `owner/repo (N)` where N is the count of active entries. This reuses the existing `ListActiveEntries` call already made by `overviewHandler` but drops the head-of-queue info.

The `RepoOverview` struct shrinks to `Owner`, `Name`, `QueueSize` — the `Head *HeadEntry` field is removed.

### 8. PR-not-in-queue returns friendly 200

**Decision**: When `/repo/{owner}/{name}/pr/{number}` is visited and the PR isn't in the queue, return HTTP 200 with a page showing "PR #N is not in the merge queue" and a breadcrumb/link back to the repo page. This is friendlier than a 404, especially for commit status links that may outlive a PR's time in the queue.

## Risks / Trade-offs

**[Gitea API dependency on PR detail page]** → The PR detail page degrades gracefully (shows "—" for title/author) if the Gitea API is down. Auto-refresh will retry on the next interval.

**[Auto-refresh API load]** → Each open PR detail tab makes one `GetPR` API call per refresh interval (default 10s). Mitigation: this is a single lightweight GET; the dashboard is typically viewed by a small team, not the public.

**[BREAKING: ExternalURL now required]** → Existing deployments without `GITEA_MQ_EXTERNAL_URL` will fail to start. Mitigation: the error message is explicit. This is a minor config addition for users — the URL is typically the same as where they access the dashboard.

**[Commit status links survive PR removal]** → When a PR leaves the queue, the `target_url` in its Gitea commit status still points to the dashboard. The friendly "not in queue" page handles this gracefully rather than showing a 404.

**[merge.StartTesting parameter growth]** → `StartTesting` already takes 6 parameters. Adding `externalURL` makes it 7. This is acceptable for now; a future refactor could introduce a Deps struct for this package too, but that's out of scope.
