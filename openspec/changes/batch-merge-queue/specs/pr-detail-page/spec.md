## ADDED Requirements

### Requirement: Batch panel on PR detail page
When a PR's entry has `active_batch_id` set, the PR detail page SHALL show a batch panel with: batch id, link to `gitea-mq/batch/<id>` on the forge, the list of co-batched PR numbers (linked), and which bucket this PR is currently in (`current` / `pending` / `landed`).

#### Scenario: PR in current
- **WHEN** PR #20 is in batch #7 with `members=[#10,#20,#30]`, `current=[#10,#20,#30]`
- **THEN** `/repo/.../pr/20` shows "Batch #7 · current" linking to `gitea-mq/batch/7` and lists #10, #30

#### Scenario: PR in pending during bisection
- **WHEN** PR #30 is in `pending=[[#30,#40]]` of batch #7
- **THEN** the panel shows "Batch #7 · pending"
