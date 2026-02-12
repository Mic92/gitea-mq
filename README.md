# gitea-mq

Status: alpha

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

All configuration is via environment variables:

| Variable | Required | Default | Description |
|---|---|---|---|
| `GITEA_MQ_GITEA_URL` | yes | — | Gitea instance URL |
| `GITEA_MQ_GITEA_TOKEN` | yes | — | API token with repo scope |
| `GITEA_MQ_REPOS` | yes | — | Comma-separated `owner/repo` list |
| `GITEA_MQ_DATABASE_URL` | yes | — | PostgreSQL connection string |
| `GITEA_MQ_WEBHOOK_SECRET` | yes | — | Shared secret for webhook HMAC |
| `GITEA_MQ_LISTEN_ADDR` | no | `:8080` | HTTP listen address |
| `GITEA_MQ_WEBHOOK_PATH` | no | `/webhook` | Webhook endpoint path |
| `GITEA_MQ_EXTERNAL_URL` | no | — | URL where Gitea can reach this service (for webhook auto-setup; without it, you must create webhooks manually) |
| `GITEA_MQ_POLL_INTERVAL` | no | `30s` | Automerge discovery poll interval |
| `GITEA_MQ_CHECK_TIMEOUT` | no | `1h` | Timeout for required checks |
| `GITEA_MQ_REQUIRED_CHECKS` | no | — | Fallback required CI contexts when branch protection has none (comma-separated) |
| `GITEA_MQ_REFRESH_INTERVAL` | no | `10s` | Dashboard auto-refresh interval |
| `GITEA_MQ_LOG_LEVEL` | no | `info` | Log level: debug, info, warn, error |

## Auto-setup

On startup, gitea-mq automatically configures each managed repository:

- **Branch protection**: Adds `gitea-mq` as a required status check to all existing branch protection rules (always runs)
- **Webhook**: Creates a webhook for `status` events pointed at the service (only if `GITEA_MQ_EXTERNAL_URL` is set)

If you set `GITEA_MQ_EXTERNAL_URL`, gitea-mq will auto-create webhooks so Gitea can push `status` events back.
This is the externally-reachable URL of gitea-mq (e.g. `https://mq.example.com`), **not** the Gitea URL.
Without it, branch protection is still auto-configured,
but you must create the webhook manually in each repo (event type: `status`, pointing at `<your-mq-url>/webhook`).

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

A minimal web dashboard is served at the root path:

- **`/`** — Overview of all repos and queue sizes
- **`/repo/{owner}/{name}`** — Queue detail with PR states and check status
- **`/healthz`** — Health check endpoint

Auto-refreshes via `<meta http-equiv="refresh">` (works without JavaScript).

## NixOS module

```nix
{
  inputs.gitea-mq.url = "github:jogman/gitea-mq";

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
### NixOS module options

| Option | Type | Default | Description |
|---|---|---|---|
| `enable` | bool | `false` | Enable the service |
| `package` | package | `pkgs.gitea-mq` | Package to use |
| `giteaUrl` | string | — | Gitea instance URL |
| `giteaTokenFile` | path | — | File containing the API token |
| `repos` | list of strings | — | Repos to manage (`owner/name`) |
| `databaseUrl` | string | `postgres:///gitea-mq?host=/run/postgresql` | PostgreSQL connection string |
| `webhookSecretFile` | path | — | File containing the webhook secret |
| `listenAddr` | string | `:8080` | HTTP listen address |
| `webhookPath` | string | `/webhook` | Webhook endpoint path |
| `externalUrl` | string | — | URL where Gitea can reach this service (for webhook auto-setup) |
| `pollInterval` | string | `30s` | Poll interval |
| `checkTimeout` | string | `1h` | Check timeout |
| `requiredChecks` | list of strings | `[]` | Fallback required CI contexts when branch protection has none |
| `refreshInterval` | string | `10s` | Dashboard refresh interval |
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
