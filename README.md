# gitea-mq

Status: beta

A merge queue for [Gitea](https://gitea.com). Serializes PR merges so your main branch stays green.

## How it works

gitea-mq hooks into Gitea's existing **"Merge when checks succeed"** (automerge) button — no bot commands needed.

1. User clicks "Merge when checks succeed" on a PR
2. gitea-mq detects the automerge via polling and enqueues the PR
3. For the head-of-queue PR, gitea-mq creates a temporary merge branch (`gitea-mq/<pr>`) merging the PR into the latest target branch
4. CI runs on the merge branch
5. If all required checks pass → gitea-mq sets its status to `success` → Gitea's automerge performs the actual merge
6. If checks fail or timeout → gitea-mq cancels automerge and posts a comment explaining why

One PR is tested at a time per target branch. PRs targeting different branches get independent queues.

## Requirements

- Gitea ≥ 1.22
- PostgreSQL
- A Gitea API token with repo read/write permissions

## Configuration

gitea-mq can manage Gitea repos, GitHub repos, or both from one process. At
least one of the two backends must be configured.


All configuration is via environment variables:

| Variable | Required | Default | Description |
|---|---|---|---|
| `GITEA_MQ_GITEA_URL` | gitea | — | Gitea instance URL. Setting this enables the Gitea backend. |
| `GITEA_MQ_GITEA_TOKEN` | gitea | — | API token with repo scope |
| `GITEA_MQ_REPOS` | no | — | Comma-separated `owner/repo` list of Gitea repos (optional when `GITEA_MQ_TOPIC` is set) |
| `GITEA_MQ_TOPIC` | no | — | Discover Gitea repos by topic instead of (or in addition to) a static list |
| `GITEA_MQ_WEBHOOK_SECRET` | gitea | — | Shared secret for the Gitea webhook HMAC |
| `GITEA_MQ_GITHUB_APP_ID` | github | — | GitHub App ID. Setting this enables the GitHub backend. |
| `GITEA_MQ_GITHUB_PRIVATE_KEY` / `_FILE` | github | — | PEM-encoded App private key, or path to a file containing it |
| `GITEA_MQ_GITHUB_WEBHOOK_SECRET` | github | — | Webhook secret configured on the GitHub App |
| `GITEA_MQ_GITHUB_REPOS` | no | — | Comma-separated `owner/repo` list of GitHub repos to manage in addition to all repos the App is installed on |
| `GITEA_MQ_GITHUB_POLL_INTERVAL` | no | `GITEA_MQ_POLL_INTERVAL` | Override the reconcile poll interval for GitHub (its rate limit is much higher) |
| `GITEA_MQ_DATABASE_URL` | yes | — | PostgreSQL connection string |
| `GITEA_MQ_LISTEN_ADDR` | no | `:8080` | HTTP listen address |
| `GITEA_MQ_WEBHOOK_PATH` | no | `/webhook` | Legacy alias for the Gitea webhook endpoint (the canonical path is `/webhook/gitea`) |
| `GITEA_MQ_EXTERNAL_URL` | yes | — | URL where Gitea can reach this service (used for webhook auto-setup and commit status target URLs) |
| `GITEA_MQ_POLL_INTERVAL` | no | `30s` | Automerge discovery poll interval |
| `GITEA_MQ_CHECK_TIMEOUT` | no | `1h` | Timeout for required checks |
| `GITEA_MQ_REQUIRED_CHECKS` | no | — | Fallback required CI contexts when branch protection has none (comma-separated) |
| `GITEA_MQ_REFRESH_INTERVAL` | no | `10s` | Dashboard auto-refresh interval |
| `GITEA_MQ_DISCOVERY_INTERVAL` | no | `5m` | How often to re-scan Gitea topics and GitHub installations |
| `GITEA_MQ_LOG_LEVEL` | no | `info` | Log level: debug, info, warn, error |

## Repo selection

You can tell gitea-mq which repos to manage in three ways:

**Static list** — enumerate repos explicitly:

```bash
GITEA_MQ_REPOS=org/app,org/lib
```

**Topic discovery** — tag repos in Gitea with a topic (e.g. `merge-queue`) and let gitea-mq find them automatically. Any repo the API token has admin access to and that carries the topic will be picked up. Repos are re-discovered periodically, so adding or removing the topic on a repo is all you need to do.

```bash
GITEA_MQ_TOPIC=merge-queue
```

**Both** — combine a static list with topic discovery. Explicitly listed repos are always managed regardless of their topics.

```bash
GITEA_MQ_REPOS=org/critical-service
GITEA_MQ_TOPIC=merge-queue
```

## GitHub setup

gitea-mq talks to GitHub as a [GitHub App](https://docs.github.com/en/apps/creating-github-apps).
Use the [App creation helper](https://mic92.github.io/gitea-mq/) to pre-fill
the registration form, or register one manually with:

- **Webhook URL**: `${GITEA_MQ_EXTERNAL_URL}/webhook/github`, secret = `GITEA_MQ_GITHUB_WEBHOOK_SECRET`
- **Permissions** (Repository): Checks **read & write**, Commit statuses **read**, Contents **read & write**, Pull requests **read & write**, Administration **read & write**, Metadata **read**
- **Subscribed events**: `pull_request`, `check_run`, `status`, `installation`, `installation_repositories`

Generate a private key, then set `GITEA_MQ_GITHUB_APP_ID` and
`GITEA_MQ_GITHUB_PRIVATE_KEY_FILE`. On startup gitea-mq patches the App's
webhook URL and secret to match `GITEA_MQ_EXTERNAL_URL` /
`GITEA_MQ_GITHUB_WEBHOOK_SECRET`, so you can leave those fields blank when
registering the App. Install the App on the orgs/repos you
want managed; gitea-mq picks up every installation automatically.
`GITEA_MQ_GITHUB_REPOS` is optional and additive: listed repos stay managed
even if the installation is later removed.

## Auto-setup

On startup, gitea-mq automatically configures each managed repository:

- **Gitea**: adds `gitea-mq` as a required status check to all existing branch protection rules and creates a `status` webhook pointed at the service
- **GitHub**: enables `allow_auto_merge` and creates a `gitea-mq` repository ruleset that requires the `gitea-mq` check on the default branch (the App and repo admins are bypass actors). Add further target branches to the ruleset's include list if you queue PRs against more than the default branch.

If the GitHub App lacks the Administration permission, auto-setup is skipped
with a warning and the queue still runs against whatever the operator
pre-configured.

`GITEA_MQ_EXTERNAL_URL` is the externally-reachable URL of gitea-mq (e.g. `https://mq.example.com`), **not** the Gitea URL.
It is used for webhook auto-setup and as the target URL in commit statuses (linking to the dashboard).

## Hiding merge branches from git clients

gitea-mq creates temporary branches under `gitea-mq/*` for CI testing. By default, these branches are visible to git clients and will be fetched by `git fetch`. To prevent this, you can configure Gitea's git to hide them from the ref advertisement.

**NixOS**: When `services.gitea` is enabled on the same host, the NixOS module automatically configures `uploadpack.hideRefs` for you. To disable this, set:

```nix
services.gitea-mq.hideRefFromClients = false;
```

**Non-NixOS**: Run the following as the Gitea system user (the user that owns the Gitea data directory):

```bash
git config --global uploadpack.hideRefs refs/heads/gitea-mq/
```

This hides the branches from `git fetch` and `git ls-remote` but does not affect the Gitea web UI, API, or webhook-driven CI. CI systems that need to check out merge branches can still fetch them with an explicit refspec (e.g., `git fetch origin gitea-mq/123`).

## CI configuration

Your CI must run on `gitea-mq/*` branches. For example, in a Woodpecker/Drone pipeline:

```yaml
when:
  branch:
    - main
    - gitea-mq/*
```

gitea-mq needs to know which CI checks must pass on the merge branch before it allows a merge. It resolves this in order:

1. **Branch protection** — If the target branch has protection rules with required status checks, those are used (excluding `gitea-mq` itself to avoid a circular dependency).
2. **`GITEA_MQ_REQUIRED_CHECKS`** — If branch protection exists but has no required status checks (or no branch protection is configured at all), this comma-separated list is used as a fallback (e.g. `ci/woodpecker,lint`).
3. **Any single success** — If neither is configured, any single passing commit status on the merge branch is enough.

## Dashboard

A lightweight web dashboard shows queue status across all managed repos, lets you drill into individual repos to see queued PRs, and inspect check results for each PR. Auto-refreshes without JavaScript. A `/healthz` endpoint is available for monitoring.

Repo and PR pages live under `/repo/{forge}/{owner}/{name}` (e.g.
`/repo/github/org/app/pr/42`). Paths without the forge segment resolve as
Gitea for compatibility with links posted by older versions.

## NixOS module

```nix
{
  inputs.gitea-mq.url = "github:Mic92/gitea-mq";

  # In your NixOS configuration:
  imports = [ inputs.gitea-mq.nixosModules.default ];

  services.gitea-mq = {
    enable = true;
    giteaUrl = "https://gitea.example.com";
    giteaTokenFile = "/run/secrets/gitea-mq-token";  # file containing the API token
    repos = [ "org/app" "org/lib" ];
    databaseUrl = "postgres:///gitea-mq?host=/run/postgresql";
    webhookSecretFile = "/run/secrets/gitea-mq-webhook-secret";
    externalUrl = "https://mq.example.com";
  };
}
```

Real-world deployments:

- Gitea backend: [clan-infra](https://git.clan.lol/clan/clan-infra/src/branch/main/modules/web01/gitea-mq.nix) · [dashboard](https://mq.clan.lol)
- GitHub backend: [Mic92/dotfiles](https://github.com/Mic92/dotfiles/blob/main/machines/eve/modules/gitea-mq.nix) · [dashboard](https://mq.thalheim.io)

### NixOS module options

| Option | Type | Default | Description |
|---|---|---|---|
| `enable` | bool | `false` | Enable the service |
| `package` | package | `pkgs.gitea-mq` | Package to use |
| `giteaUrl` | string or null | `null` | Gitea instance URL; enables the Gitea backend |
| `giteaTokenFile` | path | — | File containing the Gitea API token |
| `webhookSecretFile` | path | — | File containing the Gitea webhook secret |
| `github.appId` | int or null | `null` | GitHub App ID; enables the GitHub backend |
| `github.privateKeyFile` | path | — | File containing the GitHub App private key (PEM) |
| `github.webhookSecretFile` | path | — | File containing the GitHub App webhook secret |
| `github.repos` | list of strings | `[]` | GitHub repos in addition to all installations |
| `github.pollInterval` | string or null | `null` | Override poll interval for GitHub |
| `repos` | list of strings | `[]` | Repos to manage (`owner/name`); optional when `topic` is set |
| `topic` | string or null | `null` | Discover repos by Gitea topic |
| `databaseUrl` | string | `postgres:///gitea-mq?host=/run/postgresql` | PostgreSQL connection string |
| `listenAddr` | string | `:8080` | HTTP listen address |
| `webhookPath` | string | `/webhook` | Webhook endpoint path |
| `externalUrl` | string | — | URL where Gitea can reach this service (for webhook auto-setup and commit status links) |
| `pollInterval` | string | `30s` | Poll interval |
| `checkTimeout` | string | `1h` | Check timeout |
| `requiredChecks` | list of strings | `[]` | Fallback required CI contexts when branch protection has none |
| `refreshInterval` | string | `10s` | Dashboard refresh interval |
| `discoveryInterval` | string | `5m` | How often to re-discover repos by topic |
| `logLevel` | enum | `info` | Log level |

## Development

```bash
# Enter dev shell
nix develop

# Run tests (requires PostgreSQL, provided by the dev shell)
go test ./...

# Build
nix build

# Run NixOS integration test
nix build .#checks.x86_64-linux.nixos-test

# Format
nix fmt
```
