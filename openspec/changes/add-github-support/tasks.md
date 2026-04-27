## 1. Forge abstraction package

- [x] 1.1 Write `internal/forge/forge_test.go`: table tests for `RepoRef.String()`/`ParseRepoRef()` (`gitea:o/n`, `github:o/n`, invalid) and `Kind` constants — RED
- [x] 1.2 Implement `internal/forge/forge.go`: `Kind`, `RepoRef`, `PR`, `MQStatus`, `CheckState`, `SetupConfig`, `Forge` interface — GREEN
- [x] 1.3 Write `internal/forge/set_test.go`: `Set.Register`/`Set.For(ref)` returns correct forge, error when kind missing — RED
- [x] 1.4 Implement `internal/forge/set.go` — GREEN
- [x] 1.5 Write `internal/forge/mock.go` (`MockForge` with `Fn` fields, mirrors `gitea.MockClient` style) — no test, this is test infra
- [x] 1.6 `flake-fmt` + `go vet ./...`

## 2. Gitea adapter implements Forge

- [x] 2.1 Write `internal/gitea/forge_test.go` covering only adapter *transforms* (passthroughs to `gitea.Client` are already covered): `ListAutoMergePRs` folds timeline into `PR.AutoMergeEnabled`; `GetRequiredChecks` strips `gitea-mq`; `CreateMergeBranch` maps `MergeConflictError` → `conflict=true` — RED
- [x] 2.2 Implement `internal/gitea/forge.go` (`giteaForge` wrapping existing `Client`): `Kind`, URL helpers, `ListAutoMergePRs`, `GetPR`, `SetMQStatus`, `GetRequiredChecks`, `GetCheckStates`, `CreateMergeBranch`, `DeleteBranch`, `ListBranches`, `CancelAutoMerge`, `Comment`, `EnsureRepoSetup` — GREEN (passthroughs are one-liners, no dedicated tests)
- [x] 2.3 Move timeline-inspection logic from `internal/poller/automerge.go` into `giteaForge.ListAutoMergePRs`; keep `poller` test fixtures, assert behaviour unchanged
- [x] 2.4 Move `internal/setup` Gitea branch-protection + webhook ensure into `giteaForge.EnsureRepoSetup`; existing `setup` tests pass against adapter

## 3. Migrate callers to forge.Forge (no behaviour change)

- [x] 3.1 Extend `config.RepoRef` with `Forge forge.Kind`; `String()` = `<forge>:<owner>/<name>`; update `ParseRepoRef` callers; add tests for `GITEA_MQ_REPOS` parsing → forge `gitea`
- [x] 3.2 Write failing test: `registry` keys by `forge:owner/name` and `Deps.Forges *forge.Set` resolves correct forge in `Add` — RED
- [x] 3.3 Refactor `internal/registry`: replace `Deps.Gitea` with `Deps.Forges`; key map by `ref.String()`; call `forge.EnsureRepoSetup` — GREEN; `registry_test.go` passes
- [x] 3.4 Refactor `internal/poller`: take `forge.Forge` + `RepoRef`; call `ListAutoMergePRs`; `poller_test.go` passes with `MockForge`
- [x] 3.5 Refactor `internal/merge`: `CreateMergeBranch`/`CleanupMergeBranch` take `forge.Forge`; `merge_test.go` passes with `MockForge`
- [x] 3.6 Refactor `internal/monitor`: `SetMQStatus`, `GetRequiredChecks`, `GetCheckStates`, `CancelAutoMerge`, `Comment` via `forge.Forge`; tests pass
- [x] 3.7 Refactor `internal/discovery`: emit `forge.RepoRef{Forge: gitea}`; tests pass
- [x] 3.8 Refactor `internal/web`: use `Forge.RepoHTMLURL`/`PRHTMLURL`, `Forge.GetPR`; tests pass
- [x] 3.9 Update `cmd/` wiring: build `forge.Set` with Gitea adapter; binary builds
- [x] 3.10 `go test ./...` and `internal/integration` e2e green — Gitea behaviour unchanged

## 4. Database forge column

- [x] 4.1 Write failing store test: `GetOrCreateRepo(forge, owner, name)` distinct rows for `gitea`/`github` same `owner/name` — RED
- [x] 4.2 Add `internal/store/pg/migrations/002_add_forge.sql`: `ALTER TABLE repos ADD COLUMN forge TEXT NOT NULL DEFAULT 'gitea'`; drop old unique; add `UNIQUE(forge, owner, name)`
- [x] 4.3 Update `query.sql` (`GetOrCreateRepo`, `GetRepoByOwnerName` → take `forge`); regenerate sqlc
- [x] 4.4 Update `internal/queue` + `internal/registry` callers to pass forge — store test GREEN
- [x] 4.5 Migration test: apply `001` then `002` on testdb with seeded row → `forge='gitea'` backfilled

## 5. Config split (Gitea optional, GitHub block)

- [x] 5.1 Write `config_test.go` cases: GitHub-only OK; no-forge fails; `APP_ID` without key fails; `_FILE` read; `GITHUB_POLL_INTERVAL` inherits `POLL_INTERVAL`; `GITEA_MQ_GITHUB_REPOS` parsed as `github` refs — RED
- [x] 5.2 Refactor `internal/config`: `Config{Gitea *GiteaConfig; Github *GithubConfig; ...}`; `Load()` validates ≥1 forge — GREEN
- [x] 5.3 Update `cmd/` wiring for optional Gitea / optional GitHub; `go build` succeeds with each subset

## 6. GitHub fake server + client foundations

- [x] 6.1 `go get github.com/google/go-github/v66 github.com/bradleyfalzon/ghinstallation/v2`; `go mod tidy`
- [x] 6.2 Implement `internal/github/ghfake/server.go`: stateful `httptest.Server` with in-memory `Installs`, `Repos` (PRs, Refs, CheckRuns[sha], Rulesets, Settings); routes for `/app/installations`, `/app/installations/{id}/access_tokens`, `/installation/repositories`; helper `New()` + `AddInstallation`/`AddRepo`/`AddPR`; accept any `Authorization` header
- [x] 6.3 Extend `ghfake`: routes for `GET/POST /repos/{o}/{r}/pulls`, `GET /repos/{o}/{r}/pulls/{n}`, `POST/PATCH /repos/{o}/{r}/check-runs[/{id}]`, `GET /repos/{o}/{r}/commits/{sha}/check-runs`, `POST /repos/{o}/{r}/git/refs`, `DELETE /repos/{o}/{r}/git/refs/heads/{b}`, `POST /repos/{o}/{r}/merges` (409 when `Repo.ConflictOn[head]`), `GET /repos/{o}/{r}/rules/branches/{b}`, `GET/POST /repos/{o}/{r}/rulesets`, `PATCH /repos/{o}/{r}`, `POST /graphql` (handle `disablePullRequestAutoMerge` → clear `PR.AutoMerge`)
- [x] 6.4 Add `ghfake.Server.Client() *github.Client` helper using `WithEnterpriseURLs(srv.URL, srv.URL)` so tests get a ready client; sanity test: `AddRepo` then `client.Repositories.Get` returns it
- [x] 6.5 Write `internal/github/app_test.go` against `ghfake`: `App.Installations()` paginates; `App.ClientForRepo(owner,name)` resolves correct installation — RED
- [x] 6.6 Implement `internal/github/app.go`: JWT app transport (base URL injectable for tests), installation listing, per-install `*github.Client`, repo→install map — GREEN
- [x] 6.7 Write `internal/github/forge_test.go` against `ghfake`: `ListOpenPRs`, `GetPR` (maps `auto_merge`, `node_id`), `ListAutoMergePRs` filters `auto_merge!=nil` — RED
- [x] 6.8 Implement `internal/github/forge.go` PR methods + `Kind`/URL helpers — GREEN

## 7. GitHub forge: status, checks, branches, cancel

- [x] 7.1 Test: two `SetMQStatus` calls on same SHA result in exactly one `gitea-mq` check run server-side (assert via fake server state, not call sequence) — RED
- [x] 7.2 Implement `SetMQStatus` with state→`status/conclusion` mapping + in-mem `(repo,sha)→checkRunID` cache — GREEN
- [x] 7.3 Test: `GetRequiredChecks` reads `/rules/branches/{b}`, falls back to classic protection, excludes `gitea-mq` — RED
- [x] 7.4 Implement `GetRequiredChecks` — GREEN
- [x] 7.5 Test: `GetCheckStates` merges check-runs + commit statuses into `context→state` — RED
- [x] 7.6 Implement `GetCheckStates` — GREEN
- [x] 7.7 Test: `CreateMergeBranch` POSTs `git/refs` then `merges`; 409 → `conflict=true`; `DeleteBranch`; `ListBranches` — RED
- [x] 7.8 Implement branch ops — GREEN
- [x] 7.9 Test: `CancelAutoMerge` POSTs GraphQL `disablePullRequestAutoMerge`; "not enabled" error → nil — RED
- [x] 7.10 Implement `CancelAutoMerge` (raw `/graphql` POST) + `Comment` — GREEN

## 8. GitHub auto-setup + discovery source

- [x] 8.1 Test: `EnsureRepoSetup` PATCHes `allow_auto_merge`; creates ruleset `gitea-mq` (`~ALL`, required check `gitea-mq` with `integration_id`); idempotent on re-run; 403 → warn, no error — RED
- [x] 8.2 Implement `EnsureRepoSetup` — GREEN
- [x] 8.3 Test `internal/discovery` GitHub source: installations→repos union `Github.Repos`; static-without-install warns+skips; emits `forge=github` refs — RED
- [x] 8.4 Implement GitHub discovery source; aggregate with Gitea source in `discovery.Run`; add `TriggerNow()` channel — GREEN
- [x] 8.5 Wire GitHub forge into `forge.Set` in `cmd/`; registry `Add` per-forge poll interval

## 9. GitHub webhook endpoint

- [ ] 9.1 Test `internal/webhook`: mux serves `/webhook/gitea`, alias `{WebhookPath}`, `/webhook/github`; github 404 when unconfigured — RED
- [ ] 9.2 Implement webhook mux + per-forge handler registration — GREEN
- [ ] 9.3 Test: `X-Hub-Signature-256` validation (valid/missing/bad) — RED
- [ ] 9.4 Implement `internal/webhook/github_signature.go` — GREEN
- [ ] 9.5 Test: `pull_request` dispatch — `auto_merge_enabled`→enqueue, `auto_merge_disabled`→dequeue, `closed merged=true`→merged, `closed merged=false`→silent dequeue, `synchronize`→new-push, `labeled`→noop — RED
- [ ] 9.6 Implement `internal/webhook/github_handler.go` PR dispatch — GREEN
- [ ] 9.7 Test: `check_run completed` + `status` → monitor handler; `gitea-mq` context ignored; SHA not on merge branch ignored — RED
- [ ] 9.8 Implement check/status routing — GREEN
- [ ] 9.9 Test: `installation`/`installation_repositories` → `discovery.TriggerNow()` called — RED
- [ ] 9.10 Implement installation event routing — GREEN

## 10. Dashboard forge-aware paths

- [ ] 10.1 Test `internal/web`: `DashboardPRURL(base, forge, owner, repo, n)` includes forge segment — RED
- [ ] 10.2 Update `DashboardPRURL` + all `MQStatus` call sites — GREEN
- [ ] 10.3 Test: router serves `/repo/{forge}/{owner}/{name}` and `/repo/{forge}/{owner}/{name}/pr/{n}`; legacy 2/3-segment paths resolve as `forge=gitea`; unknown forge → 404 — RED
- [ ] 10.4 Implement route changes + template updates (forge indicator on overview, `forge:owner/name` breadcrumb, forge-host repo/PR links) — GREEN
- [ ] 10.5 Overview render test with mixed `gitea`+`github` repos: assert presence of forge badge text and `href="/repo/github/..."` / `href="/repo/gitea/..."` (targeted substring checks, no HTML snapshot)

## 11. Integration

- [ ] 11.1 Add `internal/integration/github_e2e_test.go` using `ghfake.Server` + `testutil.TestDB` + real wiring (registry/poller/webhook mux/monitor); scenario enable-auto-merge → enqueue → merge-branch → check_run success → `gitea-mq` check `success` → `closed merged` → advance — RED then GREEN against already-shipped components
- [ ] 11.2 Mixed-forge integration test: `gitea:org/app` + `github:org/app` queues independent
- [ ] 11.3 Reconcile-poll test: skip webhook, poll alone enqueues GitHub PR
- [ ] 11.4 `go test ./...` clean; existing Gitea e2e unchanged

## 12. Docs + polish

- [ ] 12.1 README: GitHub section (App registration steps, permissions: Checks RW, Contents RW, Pull requests RW, Administration RW, Metadata R; events: `pull_request`, `check_run`, `status`, `installation`, `installation_repositories`; webhook URL `{EXTERNAL_URL}/webhook/github`)
- [ ] 12.2 README: config table — add `GITEA_MQ_GITHUB_*` rows; mark Gitea vars "required if Gitea configured"
- [ ] 12.3 README: dashboard URL scheme note (`/repo/{forge}/...`, legacy fallback)
- [ ] 12.4 NixOS module: add GitHub options (`appId`, `privateKeyFile`, `webhookSecretFile`, `pollInterval`, `repos`)
- [ ] 12.5 `flake-fmt`; `go vet ./...`; `golangci-lint run` (if configured) clean
