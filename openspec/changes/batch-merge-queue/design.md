## Context

Current flow: one `queue_entries` row per `(repo, target_branch)` is `testing`; `merge.StartTesting` builds `gitea-mq/<pr>`; on green `monitor.HandleSuccess` flips the `gitea-mq` status and waits for the forge's automerge. gitea-mq never writes the target branch.

Batching changes both: the unit under test is a set of entries, and on green gitea-mq fast-forwards the target itself so the landed tree is the tested tree.

## Non-Goals

Speculative pipelining; cross-branch/repo batching; rollup PRs; adaptive sizing; preserving squash/rebase merge style for batched PRs (always merge commits — documented).

## Decisions

### D1: One live batch row per queue; bisection state is a stack on it

```sql
CREATE TYPE batch_state AS ENUM ('forming','testing','done','cancelled');

CREATE TABLE batches (
  id                 BIGSERIAL PRIMARY KEY,
  repo_id            BIGINT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  target_branch      TEXT   NOT NULL,
  state              batch_state NOT NULL DEFAULT 'forming',

  member_ids         BIGINT[] NOT NULL,           -- original FIFO set, immutable
  current_ids        BIGINT[] NOT NULL,           -- on branch_sha right now
  pending            JSONB    NOT NULL DEFAULT '[]', -- stack of BIGINT[]: halves not yet tried
  landed_ids         BIGINT[] NOT NULL DEFAULT '{}',
  ejected_ids        BIGINT[] NOT NULL DEFAULT '{}',

  branch_name        TEXT,                        -- gitea-mq/batch/<id>, reused across rebuilds
  branch_sha         TEXT,
  builds             INT NOT NULL DEFAULT 0,      -- CI runs consumed
  ff_retries         INT NOT NULL DEFAULT 0,
  flaky              BOOLEAN NOT NULL DEFAULT false,

  created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  testing_started_at TIMESTAMPTZ                  -- reset every rebuild
);
CREATE UNIQUE INDEX ux_batches_live ON batches(repo_id, target_branch)
  WHERE state IN ('forming','testing');

ALTER TABLE queue_entries ADD COLUMN active_batch_id BIGINT REFERENCES batches(id) ON DELETE SET NULL;
```

Invariant: `current ∪ flatten(pending) ∪ landed ∪ ejected == member_ids`. `active_batch_id` is set for every member not yet landed/ejected. `queue_entries.merge_branch_{name,sha}` mirror the batch's for entries in `current_ids` only; entries in `pending` have them NULL.

*Rejected*: parent/child batch tree + `batch_members` join table — required ancestor/descendant walks on every disruption, sibling-pending storage, per-child outcome tracking, and an `advance` race guard. The stack removes all four.

### D2: Mode split

`BatchMax==1` → legacy path, untouched (no `batches` row, no push). Else batch path.

Edge: batch mode forms a singleton *and* `SkipQueueIfUpToDate` fires → discard the row, run that entry through legacy. Avoids fast-forwarding `main` to a raw PR head and bypassing the repo's merge style.

### D3: `Build(b)`

```
delete b.branch_name if exists
tip, steps := stack(target, current[*].sha, b.branch_name)
for e,step in zip(current, steps):
    if step.conflict: eject(e, "Merge conflict in batch (after #<prev>)")
    if step.err:      eject(e, "Failed to create merge branch: <err>")
if surv empty: Next(b); return
persist branch_sha=tip, state='testing', testing_started_at=now(), builds++
for e in surv: e.merge_branch_* = b.branch_*
clear check_statuses(surv)  // last, so a stale save that raced before SetMergeBranch is wiped
if builds==1: SetMQStatus(pending, "Testing batch #<id> (N PRs)") on every member head
```

```go
// Forge additions
MergeInto(ctx, owner, name, branch, headSHA string) (sha string, conflict bool, err error)

type MergeStacker interface { // optional
    StackMerges(ctx, owner, name, base string, heads []string, branch string) (tip string, []MergeStep, error)
}
type MergeStep struct{ Conflict bool; Err error }
```

`stack()` type-asserts the forge for `MergeStacker`; otherwise loops `CreateMergeBranch` + `MergeInto`. GitHub uses the fallback (`POST /repos/{o}/{r}/merges` is cheap). Gitea implements `StackMerges` so the whole batch branch is built in **one** clone instead of N: clone `base`, then per head `fetch + merge --no-ff` (on conflict `merge --abort` and mark the step), push once. A whole-operation failure (clone/push) returns `err`; the engine leaves the row `forming` and the next `FormAndBuild` retries it.

### D4: `HandlePass(b)`

```
err := FastForward(target, b.branch_sha)
if ErrNotFastForward:
    ff_retries++; if <3: Build(b); else eject(current, "Target branch moved repeatedly"); Next(b); return
if permission error:
    eject(current, "gitea-mq lacks push permission on <target>: <msg>"); Next(b); return
ff_retries = 0
for e in current: SetMQStatus(success, "Merged via batch #<id>"); skipStaleMirrors; dequeue; active_batch_id=NULL
landed += current; ensureMergedOrClose(current)
Next(b)
```

```go
// Forge additions
FastForward(ctx, owner, name, branch, sha string) error  // ErrNotFastForward when sha ∉ descendants(tip)
ClosePR(ctx, owner, name string, number int64) error
```

GitHub `FastForward`: `PATCH git/refs/heads/{branch} {sha, force:false}`; 422 "not a fast forward" → `ErrNotFastForward`.

Gitea `FastForward`: shell `git` over HTTPS with the API token in-URL — same
mechanism `MergeBranches` already uses, no new dependency:

```
git init --bare -q <tmp>
git fetch -q <authed-url> <sha>
git push  <authed-url> <sha>:refs/heads/<target>   # non-force
```

A depth-limited fetch is *not* sufficient: the git client checks fast-forward
locally before pushing and needs `sha`'s ancestry back to the current target
tip. The full fetch costs the same order as `MergeBranches`' existing
single-branch clone. stderr is parsed for `non-fast-forward`/`fetch first`
→ `ErrNotFastForward` and `protected branch`/`not allowed to push` →
`PushDeniedError` (token redacted).

`ensureMergedOrClose(e)`: poll `GetPR` for `Merged=true` ≤10s; if still open, `ClosePR` + comment "Merged as `<sha>` via batch #<id>". The forge merge endpoint is *not* called — it could mint a second merge commit on top of the tested SHA.

### D5: `HandleFail(b, failedCheck)` / `Next(b)`

```
HandleFail:
  if len(current)==1: eject(current[0], failedCheck); Next(b); return
  if BisectMaxSteps>0 && builds>=BisectMaxSteps:
      eject(current ∪ flatten(pending), "Bisection limit reached"); pending=∅; Next(b); return
  left,right := halves(current)
  pending.push(right); current = left
  rebuild(b)

Next:
  if pending nonempty: current = pending.pop(); rebuild(b); return
  flaky = (ejected==∅ && builds>1)   // root failed but every slice passed
  delete branch; state='done'; advance

rebuild:
  state='forming'; Save(b); Build(b)
```

The `forming` save before every rebuild is the crash invariant: if `Build` fails mid-forge-call the next `FormAndBuild`/`ReconcileLive` retries it, and a concurrent `HandleCheck` drops on the state guard. `BisectMaxSteps` caps **builds**, so it must drain `pending` when it fires; otherwise `Next` would keep popping and rebuilding past the cap.

Every `Build` starts from the current target tip (which earlier passes just advanced), so anything landed was tested against exactly the tree it lands on. Cross-PR semantic interaction across halves still slips through — same as bors-ng; documented, mitigated by small `BATCH_MAX`. Cost: ≤ `1 + 2⌈log₂N⌉` builds per culprit.

### D6: `OnMemberRemoved(b, e)` (push / unqueue / close / retarget)

```
eject(e); drop e from current and every pending slice
if e was in current: rebuild(b)   // branch under test is stale
// else: pending only — keep testing
```

`ux_batches_live` keeps `advance` from forming a competing root.

### D7: Concurrency and check routing

One `Engine` per repo holds a **per-`target_branch`** mutex. Locked entry points: `FormAndBuild`, `HandleCheck`, `HandleTimeout`, `OnMemberRemoved`, `ReconcileLive`. Batches for different branches are independent (`ux_batches_live` is per branch); a per-repo lock would head-of-line block them on forge I/O.

`HandleCheck` is the only path from the check pipeline:

```
lock(entry.target_branch)
b := GetBatch(entry.active_batch_id)
if b.state != 'testing' || entry.merge_branch_sha != b.branch_sha: drop
SaveCheckStatus(entry.id, ...); EvaluateChecks(entry.id) → HandlePass / HandleFail / TimedOut(b)
```

Persisting **after** the guard is what stops a late event for a superseded build from polluting the ledger of the current one. Every `current` entry carries the batch SHA; right halves are `ClearMergeBranch`'d on split; `findEntryForCommit` iterates `enqueued_at` order — so the ledger consistently lands on `current[0]`. This reuse of `check_statuses` keeps the schema small; a `batch_check_statuses` table is the obvious follow-up if any of those invariants needs to change.

The poller drives each live testing batch from the **row's** `branch_sha`, pinning the representative entry's `merge_branch_sha` to that value before `ApplyCheck`, so a stale entry snapshot can never feed an old build's checks back into a new one. Mirrored `gitea-mq/<ctx>` checks land only on the representative.

### D8: Setup

`SetupConfig{BatchEnabled bool}`:
- GitHub: verify App in `gitea-mq` ruleset `bypass_actors`; add if missing.
- Gitea: log a reminder; do not mutate protections (per-pattern, may be created later — any enumeration is incomplete). `FastForward` permission errors carry the exact fix and reach every affected PR.

### D9: Reconcile

Startup `ReconcileLive`:
- Reap orphan `gitea-mq/batch/*` branches (no live row).
- Live `testing` → re-stamp `current` entries' `merge_branch_*` from the row so a crash between Build's `SaveBatch` and `SetMergeBranch` cannot strand routing.
- Live `forming` → `Build(b)` (step 1 deletes any half-built branch).

Mid-run: `FormAndBuild` also retries a `forming` live batch, so a transient `Build` error does not stall the queue until restart.

### D10: Config

| Var | Default | Meaning |
|---|---|---|
| `GITEA_MQ_BATCH_MAX` | `1` | Max PRs per root. `1` = off. `0` = unlimited. |
| `GITEA_MQ_BISECT_MAX_STEPS` | `0` | Max `builds` per root. `0` = unlimited. |

## Risks

- Pushes to protected branches (opt-in, non-force, actionable error on deny).
- Batched PRs land as merge commits regardless of repo merge style.
- Cross-PR interaction across bisection halves can land both (bors-ng parity).
- `jsonb` stack is opaque to SQL — acceptable; only `internal/batch` reads it.


