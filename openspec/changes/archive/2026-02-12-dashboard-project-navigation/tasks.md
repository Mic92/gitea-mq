## 1. Config: Make ExternalURL required (TDD)

- [x] 1.1 Add test in `internal/config/` verifying that `Load()` fails when `GITEA_MQ_EXTERNAL_URL` is unset and succeeds when set. Verify trailing slash is stripped. (Test fails — ExternalURL not yet required.)
- [x] 1.2 In `internal/config/config.go`, move `ExternalURL` parsing before the `missing` check and add it to the required variables list. Strip trailing slash with `strings.TrimRight(cfg.ExternalURL, "/")`. (Tests pass.)

## 2. DashboardPRURL helper (TDD)

- [x] 2.1 Add unit test for `DashboardPRURL(baseURL, owner, repo string, prNumber int64) string` in `internal/gitea/` verifying URL format (e.g., `https://mq.example.com/repo/org/app/pr/42`) and trailing-slash handling on `baseURL`. (Test fails — function doesn't exist.)
- [x] 2.2 Implement `DashboardPRURL` helper in `internal/gitea/client.go`. (Test passes.)

## 3. MQStatus signature change

- [x] 3.1 Change `MQStatus` signature to `MQStatus(state, description, targetURL string) CommitStatus` — set `TargetURL` field. (Callers won't compile yet — that's expected. No standalone test needed; TargetURL correctness is verified at every call site in §5–§7.)

## 4. Deps struct plumbing

- [x] 4.1 Add `ExternalURL string` field to `registry.Deps` in `internal/registry/registry.go`.
- [x] 4.2 Add `ExternalURL string` field to `monitor.Deps` in `internal/monitor/monitor.go`. Propagate from `registry.Add()`.
- [x] 4.3 Add `ExternalURL string` field to `poller.Deps` in `internal/poller/poller.go`. Propagate from `registry.Add()`.
- [x] 4.4 Fix all existing tests that construct Deps structs — add `ExternalURL` field so they compile. Affected files: `monitor_test.go`, `poller_test.go`, `webhook_test.go`, `registry_test.go`, `discovery_test.go`.

## 5. Commit status target_url in merge package (TDD)

- [x] 5.1 Update `merge.StartTesting()` tests in `merge_test.go` first: assert that the 3 `CommitStatus` values posted by `StartTesting` now include the correct `TargetURL`. Change the expected call signatures to pass `externalURL`. (Tests fail — signature unchanged.)
- [x] 5.2 Update `merge.StartTesting()` signature to accept `externalURL string` parameter. Update its 3 `MQStatus` call sites to pass `gitea.DashboardPRURL(externalURL, owner, repo, entry.PrNumber)`. (Tests pass.)

## 6. Commit status target_url in monitor package (TDD)

- [x] 6.1 Update `monitor_test.go` first: assert that `HandleSuccess` and `removeFromQueue` post statuses with the correct `TargetURL`. (Tests fail — monitor doesn't pass URLs yet.)
- [x] 6.2 Update `monitor.go` — `HandleSuccess` and `removeFromQueue`: build target URL from `deps.ExternalURL` and pass to `MQStatus`. (Tests pass.)

## 7. Commit status target_url in poller package (TDD)

- [x] 7.1 Update `poller_test.go` first: assert that enqueue pending status and automerge timeout status include the correct `TargetURL`. Assert `StartTesting` is called with `deps.ExternalURL`. (Tests fail.)
- [x] 7.2 Update `poller.go` — enqueue pending status and automerge timeout: build target URL from `deps.ExternalURL` and pass to `MQStatus`. Update `StartTesting` call to pass `deps.ExternalURL`. (Tests pass.)

## 8. Wire ExternalURL through main.go

- [x] 8.1 Update `main.go` to pass `cfg.ExternalURL` to `registry.Deps`.
- [x] 8.2 Update `integration/e2e_test.go` — add `ExternalURL` to Deps struct construction. Assert `TargetURL` is non-empty (URL format already covered by unit tests in §5–§7).

## 9. Web handler: Add Gitea client to Deps (TDD)

- [x] 9.1 Add `Gitea gitea.Client` field to `web.Deps`. Update `main.go` to pass `giteaClient` to `webDeps`.

## 10. Web handler: Simplify data types

- [x] 10.1 Remove `HeadEntry` struct and `Head *HeadEntry` field from `RepoOverview`. The overview only needs `Owner`, `Name`, `QueueSize`.
- [x] 10.2 Remove `HeadPR int64` and `CheckStatuses []pg.CheckStatus` fields from `RepoDetailData`. The repo page no longer shows checks.
- [x] 10.3 Add `PRDetailData` struct with fields: `Owner`, `Name`, `PrNumber int64`, `Title string`, `Author string`, `State string`, `Position int`, `EnqueuedAt`, `CheckStatuses []pg.CheckStatus`, `InQueue bool`, `RefreshInterval int`.

## 11. Web handler: Refactor overview page (TDD)

- [x] 11.1 Update `TestOverviewShowsRepoAndQueueData` first: remove assertions for "PR #42" and head-of-queue info. Assert queue count badge and repo links instead. (Test fails — handler still returns old data.)
- [x] 11.2 Simplify `overviewHandler` — remove head-of-queue lookup logic, keep only `QueueSize` from `ListActiveEntries`.
- [x] 11.3 Update `overview.html` template: remove "Queue Size" and "Head of Queue" table columns. Show each repo as a link with a queue count badge. Replace the "No managed repositories." message with "No repositories discovered yet. Configure GITEA_MQ_REPOS or set the topic on your repos."
- [x] 11.4 Add breadcrumb `<nav>` to `overview.html` showing `gitea-mq` as plain text (current page). (Test passes.)

## 12. Web handler: Refactor repo detail page (TDD)

- [x] 12.1 Update `TestRepoDetailShowsPRsAndChecks` first: rename to `TestRepoDetailShowsPRs`, remove check-status assertions (✅ ⏳ ❌, ci/build etc.), assert PR links to `/repo/{owner}/{name}/pr/{number}` instead. (Test fails.)
- [x] 12.2 Simplify repo-list branch of `repoHandler` — remove check-status fetching logic (no `GetCheckStatuses` call, no `HeadPR`).
- [x] 12.3 Update `repo.html` template: remove check statuses section. Make each PR number a link to `/repo/{owner}/{name}/pr/{number}`. Add queue position column. Replace `<p class="back">` with breadcrumb `<nav>`: `gitea-mq` (link to `/`) › `owner/repo` (plain text). (Test passes.)

## 13. Web handler: PR detail page routing (TDD)

- [x] 13.1 Add table-driven test for invalid PR paths: `/pr/abc`, `/pr/-1`, `/pr/0`, `/repo/org/unknown/pr/42` — assert HTTP 404 for each. (The unknown-repo case for the repo page itself is already covered by `TestRepoDetailUnknownRepoReturns404`; this tests it through the PR route.)
- [x] 13.2 Refactor `repoHandler` to detect `/pr/{number}` suffix after `{name}` and dispatch to either repo-list rendering or PR-detail rendering. If the path has extra segments that don't match `/pr/{number}`, return 404. (Routing tests pass.)

## 14. Web handler: PR detail page rendering (TDD)

- [x] 14.1 Add test: head-of-queue PR in testing state — assert title, author, state, checks table with icons. (Test fails — handler not implemented.)
- [x] 14.2 Add test: non-head PR in queued state — assert title, author, state, position, no checks section.
- [x] 14.3 Add test: PR not in queue — assert HTTP 200, "not in the merge queue" message.
- [x] 14.4 Add test: Gitea API failure — assert page renders with "—" placeholders for title/author, rest of data intact.
- [x] 14.5 Implement PR-detail rendering branch in the refactored `repoHandler`: parse PR number, look up repo in managed set, look up entry via `GetEntry`, determine queue position from `ListActiveEntries`, fetch PR title/author from `deps.Gitea.GetPR()` (graceful degradation on error), fetch check statuses if head-of-queue in testing state.
- [x] 14.6 Create `pr.html` template: breadcrumb nav (`gitea-mq` › `owner/repo` › `PR #N`), PR metadata (number, title, author, state, position, enqueued time), conditional check statuses table with visual indicators, auto-refresh meta tag.
- [x] 14.7 Create `pr_not_in_queue.html` template (or conditional block in `pr.html`): "PR #N is not in the merge queue" message with breadcrumb back to repo page, auto-refresh meta tag. (All PR detail tests pass.)

## 15. Breadcrumb styling

- [x] 15.1 Add `.breadcrumb` styles to `style.css` (nav element, separator styling, link vs plain text distinction). Remove `.back` styles if unused.

## 16. Integration verification

- [x] 16.1 Run full test suite (`go test ./...`) — verify all existing and new tests pass.
- [x] 16.2 Verify `go vet ./...` and any linters pass with no new warnings.
