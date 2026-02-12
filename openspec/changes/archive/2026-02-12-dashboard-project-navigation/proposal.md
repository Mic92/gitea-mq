## Why

The current dashboard mixes concerns: the overview page shows repositories alongside queue metadata (queue size, head-of-queue PR),
and check statuses are only visible as a sub-section on the repo detail page.
This makes the UI cluttered and forces users to mentally map three different levels of information (project → PRs → checks) onto just two pages.
Separating these into a clear three-level drill-down — projects → queued PRs → pending checks — makes each page focused and scannable.

## What Changes

- The overview page (`/`) becomes a project list showing registered projects with a small queue count badge (e.g., "myorg/repo (3)"), but no head-of-queue or state details.
- A new per-project page (`/repo/{owner}/{name}`) lists only active pull requests (queued + testing) for that project, with their position, state, and basic info. No PR titles — just PR number, target branch, and state.
- A new per-PR page (`/repo/{owner}/{name}/pr/{number}`) shows PR number, title, author, state, queue position, and enqueued time. Title and author are fetched from the Gitea API on each page load (single API call, acceptable for a single-PR view). For the head-of-queue PR in "testing" state, check statuses are also displayed. For non-head PRs, the checks section is absent.
- The existing repo detail page is split: PR list stays on the repo page, check statuses move to the new PR detail page.
- Visiting a PR detail page for a PR not in the queue returns a friendly 200 page with a "PR not in queue" message and a link back to the repo page.
- All pages use breadcrumb navigation (e.g., "Overview › myorg/repo › PR #42") instead of simple back links.
- The overview page shows a helpful setup hint when no projects are registered (e.g., "No repositories discovered yet. Configure GITEA_MQ_REPOS or set the topic on your repos.") instead of the current bare "No managed repositories." message.
- The `gitea-mq` commit status posted on PRs includes a `target_url` pointing to the PR's dashboard page (`/repo/{owner}/{name}/pr/{number}`), so users can click through from Gitea directly to the merge queue dashboard.

## Capabilities

### New Capabilities
- `pr-detail-page`: A dedicated page for viewing a single PR's check statuses and queue state, served at `/repo/{owner}/{name}/pr/{number}`.

### Modified Capabilities
- `web-dashboard`: The overview page is simplified to show only the project list (no queue size or head-of-queue columns).
   The repo detail page drops the check statuses section (moved to pr-detail-page) and focuses solely on listing queued PRs with links to individual PR pages.
- `check-monitoring`: The commit status lifecycle reporting requirement is extended — all `gitea-mq` commit statuses posted on a PR's head commit SHALL include a `target_url` pointing to the PR's dashboard page. The `CommitStatus.TargetURL` field already exists but is unused; `MQStatus` gains a URL parameter.

## Impact

- **Templates**: `overview.html` simplified (remove queue size / head columns), `repo.html` refactored (remove check section, add PR links), new `pr.html` template.
- **Handlers**: `handler.go` gains a new `prHandler` for the `/repo/{owner}/{name}/pr/{number}` route; `overviewHandler` is simplified; `repoHandler` drops check-status logic.
- **Data types**: New `PRDetailData` struct; `OverviewData`/`RepoOverview` shrink; `RepoDetailData` drops check fields.
- **Spec**: `web-dashboard/spec.md` requirements rewritten; new `pr-detail-page/spec.md` added.
- **Commit statuses**: `MQStatus()` helper updated to accept and populate `TargetURL`; all callers (queue advancement, check monitoring, timeout handling) pass the dashboard URL. Requires the web dashboard's external base URL to be available where statuses are posted.
- **Config**: **BREAKING** — `GITEA_MQ_EXTERNAL_URL` becomes required (was optional). The service will fail to start if unset. This URL is used both for webhook auto-setup and for constructing dashboard links in commit statuses.
- **Gitea API**: The PR detail page makes a `GET /repos/{owner}/{repo}/pulls/{number}` call to fetch PR title and author on each page load.
- **No database schema changes** — all data is already available via existing queries; PR title/author comes from the Gitea API at render time.
- **No breaking URL changes** — URL structure is additive (`/repo/{owner}/{name}/pr/{number}` is new); existing `/` and `/repo/{owner}/{name}` paths are preserved, just with different content.
