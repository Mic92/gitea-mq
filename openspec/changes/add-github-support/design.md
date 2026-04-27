## Context

Today every consumer of repo/PR/status operations depends on `internal/gitea.Client` directly: `registry`, `poller`, `merge`, `monitor`, `setup`, `webhook`, `web`. The `RepoRegistry` keys repos by `"owner/name"` and the `repos` table has `UNIQUE(owner, name)`. Configuration assumes a single Gitea instance (`GiteaURL`, `GiteaToken`, one `WebhookSecret`). Adding GitHub means a second forge must coexist in the same process, with the same `owner/name` potentially existing on both forges.

## Goals / Non-Goals

**Goals:**
- One `gitea-mq` process manages Gitea and GitHub repos concurrently.
- Queue/merge/monitor logic is forge-agnostic; only a thin client layer differs.
- GitHub path is webhook-first with a reconcile poll safety net.
- Zero behaviour change for existing Gitea-only deployments after upgrade + migration.

**Non-Goals:**
- GitHub Enterprise Server (base URL hard-coded to `https://api.github.com`).
- PAT auth for GitHub.
- Supporting GitHub's native merge queue or merge groups.
- Renaming the project / env prefix.
- Multiple Gitea or multiple GitHub instances in one process.

## Decisions

### D1: `forge.Forge` interface + per-repo provider lookup

Introduce `internal/forge`:

```go
type Kind string
const (
    KindGitea  Kind = "gitea"
    KindGithub Kind = "github"
)

type RepoRef struct {
    Forge Kind
    Owner string
    Name  string
}

type Forge interface {
    Kind() Kind
    RepoHTMLURL(owner, name string) string
    PRHTMLURL(owner, name string, number int64) string
    BranchHTMLURL(owner, name, branch string) string

    ListOpenPRs(ctx, owner, name string) ([]PR, error)
    GetPR(ctx, owner, name string, number int64) (*PR, error)
    ListAutoMergePRs(ctx, owner, name string) ([]PR, error) // reconcile

    SetMQStatus(ctx, owner, name, sha string, st MQStatus) error
    MirrorCheck(ctx, owner, name, sha, context, state, desc, url string) error
    GetRequiredChecks(ctx, owner, name, branch string) ([]string, error)
    GetCheckStates(ctx, owner, name, sha string) (map[string]Check, error)

    CreateMergeBranch(ctx, owner, name, base, headSHA, branch string) (sha string, conflict bool, err error)
    DeleteBranch(ctx, owner, name, branch string) error
    ListBranches(ctx, owner, name string) ([]string, error)

    CancelAutoMerge(ctx, owner, name string, number int64) error
    Comment(ctx, owner, name string, number int64, body string) error

    EnsureRepoSetup(ctx, owner, name string, cfg SetupConfig) error
}
```

`forge.PR` carries `AutoMergeEnabled bool` so callers no longer parse Gitea timeline comments themselves; the Gitea adapter folds timeline inspection into `ListOpenPRs`/`ListAutoMergePRs`/`GetPR`.

`MirrorCheck` and the richer `GetCheckStates` (returning `{State, Description, TargetURL}` per context) exist so the existing status-mirroring UX — copying merge-branch CI results onto the PR head as `gitea-mq/<ctx>` and clearing/skipping stale mirrors on retest/success — can be expressed forge-agnostically. On GitHub these map to per-context check runs.

A `forge.Set` holds `map[Kind]Forge` and resolves a `Forge` from a `RepoRef`. `registry.Deps.Gitea` is replaced by `Forges *forge.Set`.

*Alternative considered*: keep `gitea.Client` as the interface and make GitHub implement it. Rejected — the interface leaks Gitea concepts (timeline comments, branch-protection edit shape, per-repo webhook CRUD) that have no GitHub analogue, and would force the GitHub impl to fake them.

### D2: `config.RepoRef` gains `Forge`; registry keyed by `forge:owner/name`

`RepoRef.String()` becomes `"<forge>:<owner>/<name>"`. `RepoRegistry.repos` keys on that. `GITEA_MQ_REPOS` continues to parse as Gitea refs; `GITEA_MQ_GITHUB_REPOS` parses as GitHub refs. No prefixed-string parsing exposed to users.

### D3: Database — additive `forge` column

New migration:

```sql
ALTER TABLE repos ADD COLUMN forge TEXT NOT NULL DEFAULT 'gitea';
ALTER TABLE repos DROP CONSTRAINT repos_owner_name_key;
ALTER TABLE repos ADD CONSTRAINT repos_forge_owner_name_key UNIQUE (forge, owner, name);
```

`DEFAULT 'gitea'` backfills existing rows; the default stays so Gitea-only operators need no action. sqlc queries that look up by owner/name gain a `forge` parameter.

*Alternative*: separate `github_repos` table. Rejected — queue/monitor code would need parallel queries everywhere.

### D4: GitHub client — `go-github` + `ghinstallation`

`go-github/v84` (the version `ghinstallation/v2` itself depends on) is used so the binary carries one go-github version, not two.

`internal/github`:
- App JWT via `github.com/bradleyfalzon/ghinstallation/v2` wrapping `net/http.Transport`; it handles installation-token caching/refresh.
- One `*github.Client` per installation, created lazily and cached by installation ID.
- An app-level client (JWT) lists installations and their repos for discovery.

*Alternative*: hand-rolled JWT + token refresh. Rejected — `ghinstallation` is small, well-maintained, and removes a class of expiry bugs.

### D5: GitHub auto-merge as enqueue signal — webhook primary, poll fallback

Primary: GitHub App webhook (single URL configured on the App) delivers `pull_request` events. Actions handled:
- `auto_merge_enabled` → enqueue
- `auto_merge_disabled` → dequeue (user-cancelled)
- `closed` with `merged=true` → mark merged, advance queue
- `synchronize` → if queued, update head SHA / restart if testing

Fallback reconcile: per managed GitHub repo, the existing poller infrastructure runs `ListAutoMergePRs` (GraphQL `autoMergeRequest` or REST `GET /pulls` then filter `auto_merge != null`) at `GITEA_MQ_POLL_INTERVAL`. The poller becomes forge-agnostic: it asks the repo's `Forge` for the current auto-merge set and diffs against the queue, exactly as it does for Gitea today. This means the Gitea timeline-specific logic moves *into* the Gitea adapter behind `ListAutoMergePRs`.

*Alternative*: webhook-only for GitHub. Rejected per operator requirement — GitHub webhook delivery has had outages; reconcile gives recovery after downtime without user action.

### D6: Status reporting via Checks API

`github.Forge.SetMQStatus` creates/updates a **check run** named `gitea-mq` on the head SHA:
- `queued` → `status=queued`
- `pending` (testing) → `status=in_progress`
- `success` → `status=completed, conclusion=success`
- `failure` → `status=completed, conclusion=failure`
- `error` → `status=completed, conclusion=cancelled` (with error text in `output.summary`)

`details_url` = dashboard PR URL. The adapter remembers the check-run ID per `(repo, sha)` in memory; if missing it lists check runs for the SHA filtered by name and updates, else creates.

*Alternative*: commit status API. Rejected — Checks API is the GitHub-App-native surface, renders richer UI, and rulesets reference checks by `{context, app_id}` which fits the App identity.

### D7: Required-check discovery & auto-setup via repository rulesets

`GetRequiredChecks` on GitHub:
1. `GET /repos/{o}/{r}/rules/branches/{branch}` → collect `required_status_checks` rule contexts.
2. Fallback: classic `GET /repos/{o}/{r}/branches/{branch}/protection` if rulesets return nothing (read-only, never written).
3. Exclude `gitea-mq` itself.

`EnsureRepoSetup` on GitHub:
1. `PATCH /repos/{o}/{r}` → `allow_auto_merge: true`.
2. List repo rulesets; find one named `gitea-mq`. If absent, create a ruleset with target `{include: ["~ALL"], exclude: ["refs/heads/gitea-mq/**"]}` containing a single `required_status_checks` rule with `{context: "gitea-mq", integration_id: <app_id>}`, plus the App as an `Integration` bypass actor. The exclusion is required so the rule does not gate `CreateMergeBranch` on the very check it produces. If present, leave it untouched (idempotent by name).
3. No webhook creation — the App-level webhook covers all installations.

*Alternative*: mutate classic branch protection. Rejected — rulesets are GitHub's forward path, can be additive without disturbing existing protection, and avoid the brittle full-object `PUT` of the classic API.

### D8: Merge-branch creation on GitHub

`CreateMergeBranch(base, headSHA, branch)`:
1. `POST /repos/{o}/{r}/merges` with `{base: branch-tmp?}` — but that endpoint merges *into an existing branch*. Instead:
   1. `POST /repos/{o}/{r}/git/refs` → create `refs/heads/gitea-mq/<n>` at current `base` tip.
   2. `POST /repos/{o}/{r}/merges` with `{base: "gitea-mq/<n>", head: <headSHA>}` → returns merge commit SHA or `409 Conflict`.
2. On `409` return `conflict=true`; caller posts failure as today.

Same two-step shape Gitea's `MergeBranches` already performs internally, so the `Forge` method signature stays identical.

### D9: Cancel auto-merge via GraphQL

REST has no "disable auto-merge" endpoint. Use GraphQL mutation `disablePullRequestAutoMerge(pullRequestId: ID!)`. The adapter resolves the PR node ID once (cached on `forge.PR`) and issues the mutation. `go-github`'s `githubv4` sibling is overkill; send a raw POST to `/graphql` with the installation token.

### D10: Webhook routing

`internal/webhook` gains a small mux:
- `POST /webhook/gitea` and legacy `POST {WebhookPath}` → existing Gitea handler (`X-Gitea-Signature`, `X-Gitea-Event`).
- `POST /webhook/github` → new handler validating `X-Hub-Signature-256` against `GITEA_MQ_GITHUB_WEBHOOK_SECRET`, dispatching on `X-GitHub-Event`.

Both handlers resolve the repo to a `forge.RepoRef`, look it up in the registry, and call the same forge-agnostic `monitor`/`queue` entry points. `installation` / `installation_repositories` events trigger an immediate discovery refresh.

### D11: Configuration

```go
type Config struct {
    Gitea  *GiteaConfig  // nil if unconfigured
    Github *GithubConfig // nil if unconfigured
    // shared fields unchanged: DatabaseURL, ListenAddr, ExternalURL, ...
}

type GiteaConfig struct {
    URL, Token, WebhookSecret, Topic string
    Repos []RepoRef
}

type GithubConfig struct {
    AppID         int64
    PrivateKey    []byte        // from GITEA_MQ_GITHUB_PRIVATE_KEY or _FILE
    WebhookSecret string
    Repos         []RepoRef     // optional static list, unioned with installations
    PollInterval  time.Duration // GITEA_MQ_GITHUB_POLL_INTERVAL; 0 = inherit GITEA_MQ_POLL_INTERVAL
}
```

`Load()` requires `Gitea != nil || Github != nil`. `GITEA_MQ_WEBHOOK_SECRET` keeps its meaning (Gitea); GitHub uses its own secret because the App webhook is configured once at App registration and may differ.

### D12: Discovery loop becomes multi-source

`internal/discovery` already produces a desired repo set and reconciles the registry. It is refactored to aggregate:
- Gitea static + topic (unchanged, now emits `RepoRef{Forge: gitea}`).
- GitHub static + installations: app-JWT client → `GET /app/installations` → per installation `GET /installation/repositories`. The managed set is the **union** of installation repos and `GithubConfig.Repos`, mirroring Gitea semantics. A statically-listed repo with no covering installation cannot be acted on (no token); discovery logs a warning and skips it. Operators restrict the managed set by scoping the App installation, not via config.

`installation*` webhooks poke the discovery loop to run immediately instead of waiting for the interval.

## Risks / Trade-offs

- **Interface churn across most packages** → mitigate by landing `internal/forge` + Gitea adapter first with all callers migrated and tests green, before any GitHub code exists.
- **Rulesets require `Administration: write` App permission**, which some operators may hesitate to grant → document required permissions; if the API returns 403, log a warning and skip auto-setup (queue still works if the operator adds the required check manually).
- **`allow_auto_merge` repo setting may be org-policy-locked** → treat `PATCH` 403/422 as non-fatal, surface in logs/dashboard.
- **Check-run ID cache is in-memory** → on restart the first `SetMQStatus` per SHA does a list-then-update; acceptable, no persistence needed.
- **GraphQL dependency for one mutation** → tiny hand-rolled client; if it breaks, fallback is "post a comment asking the user to disable auto-merge" (already the failure-comment path).
- **Rate limits** (5k/hr/installation) → reconcile poll at default 30s across many repos could approach limits on large installations. Mitigate: GitHub poll uses `GET /repos/{o}/{r}/pulls?state=open` once per repo per tick (cheap), and conditional requests (ETag) via `go-github`'s built-in caching transport.

## Migration Plan

1. Ship `internal/forge` + Gitea adapter + caller refactor (no functional change). Integration tests must pass unchanged.
2. Ship DB migration `002_add_forge.sql` (additive, default `'gitea'`).
3. Ship `internal/github`, config, webhook route, discovery source.
4. Ship dashboard/PR-page forge indicators.
5. Docs: README config table, GitHub App registration guide (permissions, webhook URL, events).

Rollback: each step is independently revertible; the DB migration is additive so downgrading the binary without reverting the migration is safe.

## Open Questions

None.
