## ADDED Requirements

### Requirement: Incremental merge into existing branch
`Forge` SHALL provide `MergeInto(owner, name, branch, headSHA) (sha, conflict, err)` that merges `headSHA` into an existing branch and returns the new tip, or `conflict=true` on merge conflict.

#### Scenario: Clean incremental merge
- **WHEN** `MergeInto("o","r","gitea-mq/batch/7", shaB)` is called and no conflict exists
- **THEN** the branch tip advances and the new SHA is returned

#### Scenario: Conflicting incremental merge
- **WHEN** `MergeInto` hits a conflict
- **THEN** it returns `conflict=true` and `err=nil`

### Requirement: Non-force fast-forward of protected branch
`Forge` SHALL provide `FastForward(owner, name, branch, sha) error` that updates `branch` to `sha` without force. It SHALL return `ErrNotFastForward` when `sha` is not a descendant of the current tip, and a distinct error when the push is rejected by branch protection.

#### Scenario: GitHub fast-forward
- **WHEN** the GitHub adapter fast-forwards `main` to a descendant SHA
- **THEN** it issues `PATCH git/refs/heads/main {sha, force:false}` and returns nil

#### Scenario: Gitea fast-forward
- **WHEN** the Gitea adapter fast-forwards `main`
- **THEN** it performs a go-git in-memory smart-HTTP push as the token user with a non-force refspec

#### Scenario: Not a fast-forward
- **WHEN** `sha` is not a descendant of the branch tip
- **THEN** `FastForward` returns `ErrNotFastForward`

### Requirement: Close pull request
`Forge` SHALL provide `ClosePR(owner, name, number) error` that closes an open PR without merging it. Closing an already-closed or already-merged PR SHALL return nil.

#### Scenario: Close after batch landing
- **WHEN** a PR's commits are on the target branch but the forge has not marked it merged
- **AND** `ClosePR` is called
- **THEN** the PR is closed and the call returns nil
