## Why

One-at-a-time testing costs N CI runs and N× latency for N queued PRs. bors-ng batches: test all queued PRs as one tree, fast-forward target on green, bisect on red. This change brings that to gitea-mq.

## What Changes

- When a queue's head slot becomes free, gitea-mq forms a **batch** from up to `GITEA_MQ_BATCH_MAX` queued PRs (FIFO order) instead of taking one. Default `1` preserves current behaviour exactly; `0` means "everything currently queued".
- A batch is tested on a single merge branch `gitea-mq/batch/<id>` built by merging each PR's head, in queue order, onto the current target tip. A PR that conflicts during construction is ejected (automerge cancelled, comment posted) and construction continues with the rest.
- On **success**, gitea-mq **fast-forwards the target branch** to the tested batch SHA (true bors semantics: what was tested is exactly what lands), sets `gitea-mq=success` on each head, and confirms each PR shows merged (short poll, else close + comment with the landing SHA).
- On **failure/timeout**, gitea-mq **bisects** using the batch row as a work-stack: split `current` in half, push the second half onto `pending`, rebuild+retest the first half from the *current* target tip; a passing half is **landed immediately**, a failing singleton is ejected, then the next pending half is popped. `GITEA_MQ_BISECT_MAX_STEPS` (default `0` = unlimited) caps total CI builds per root.
- `GITEA_MQ_SKIP_QUEUE_IF_UP_TO_DATE`: a singleton batch that qualifies falls back to the legacy forge-automerge path so the repo's configured merge style is respected.
- New `Forge` operations: `MergeInto`, `FastForward` (non-force ref update; Gitea via `git push` over HTTPS like `MergeBranches`, GitHub via refs API), `ClosePR`. GitHub auto-setup verifies the App is a ruleset bypass actor; Gitea push-whitelist is documented as an operator prerequisite and surfaced as an actionable per-PR error if missing.
- Dashboard groups the active batch visually and the PR detail page lists co-batched PRs and bisection progress.

## Capabilities

### Modified Capabilities

- `forge-abstraction`: `Forge` gains `MergeInto`, `FastForward`, `ClosePR`; `ErrNotFastForward` sentinel.
- `queue-management`: Head-of-queue becomes a *batch* of one or more entries with a stack-based bisection lifecycle on a single `batches` row; fast-forward merge replaces "wait for forge automerge" when batching is enabled.
- `check-monitoring`: Check evaluation runs against the batch branch SHA; timeout keys on the batch's `testing_started_at` (reset per rebuild); status writes capped at three per member regardless of bisection depth.
- `automerge-integration`: On batch success gitea-mq performs the merge itself and closes PRs the forge does not auto-detect as merged. Single-PR (`BATCH_MAX=1`) path keeps the existing "let forge automerge" behaviour for zero-risk default.
- `web-dashboard`: Repo page groups testing entries under their batch with the shared branch/SHA, build count, and current/pending sizes.
- `pr-detail-page`: Shows batch ID, co-batched PR numbers, and whether this PR is in `current`, `pending`, `landed`, or `ejected`.
- `repo-discovery` (setup only): GitHub `EnsureRepoSetup` verifies the App is a ruleset bypass actor when batching is enabled.

## Impact

- **Code**: new `internal/batch` (form/build/pass/fail/next loop, startup reconcile); `gitea.Client` gains `FastForwardRef`/`EditIssueState`; changes in `internal/{queue,merge,monitor,poller,registry,config,web}`; `forge.Forge` gains `MergeInto`/`FastForward`/`ClosePR`; both adapters and `MockForge` updated.
- **Database**: one new `batches` table (arrays + jsonb stack); `queue_entries` gains `active_batch_id` FK; partial unique index ensuring one live batch per queue. Additive migration.
- **Config**: `GITEA_MQ_BATCH_MAX` (default `1`), `GITEA_MQ_BISECT_MAX_STEPS` (default `0`).
- **Permissions**: Gitea token user must be in the push whitelist of protected target branches when `BATCH_MAX != 1` (documented; failure surfaces per-PR). GitHub App already bypasses via ruleset.
