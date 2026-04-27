## MODIFIED Requirements

### Requirement: HTTP webhook endpoint
The system SHALL expose per-forge HTTP webhook endpoints: `POST /webhook/gitea` for Gitea events and `POST /webhook/github` for GitHub App events. The legacy configurable path (default `/webhook`) SHALL remain as an alias for the Gitea endpoint. Endpoints for unconfigured forges SHALL respond `404`.

#### Scenario: Valid webhook POST
- **WHEN** Gitea sends a POST request to `/webhook/gitea` with a valid event payload
- **THEN** the system responds with HTTP 200
- **AND** processes the event asynchronously

#### Scenario: Legacy alias
- **WHEN** Gitea sends a POST request to `/webhook` with a valid event payload
- **THEN** it is handled identically to `/webhook/gitea`

#### Scenario: GitHub endpoint
- **WHEN** GitHub sends a POST request to `/webhook/github` with a valid event payload
- **THEN** the system responds with HTTP 200
- **AND** processes the event asynchronously

#### Scenario: Non-POST request
- **WHEN** a GET request is sent to `/webhook/gitea`
- **THEN** the system responds with HTTP 405 Method Not Allowed

#### Scenario: GitHub endpoint when GitHub unconfigured
- **WHEN** GitHub is not configured
- **AND** a POST is sent to `/webhook/github`
- **THEN** the system responds with HTTP 404

### Requirement: HMAC signature validation
The system SHALL validate webhook signatures per forge. The Gitea endpoint SHALL validate `X-Gitea-Signature` (HMAC-SHA256, hex) against `GITEA_MQ_WEBHOOK_SECRET`. The GitHub endpoint SHALL validate `X-Hub-Signature-256` (HMAC-SHA256, `sha256=`-prefixed hex) against `GITEA_MQ_GITHUB_WEBHOOK_SECRET`. Requests with missing or invalid signatures SHALL be rejected.

#### Scenario: Valid signature
- **WHEN** a webhook request arrives at `/webhook/gitea` with a valid HMAC-SHA256 signature in `X-Gitea-Signature`
- **THEN** the system accepts and processes the request

#### Scenario: Valid GitHub signature
- **WHEN** a webhook request arrives at `/webhook/github` with a valid `X-Hub-Signature-256` header
- **THEN** the system accepts and processes the request

#### Scenario: Missing signature header
- **WHEN** a webhook request arrives at `/webhook/gitea` without an `X-Gitea-Signature` header
- **THEN** the system responds with HTTP 401 Unauthorized
- **AND** does NOT process the event

#### Scenario: Invalid signature
- **WHEN** a webhook request arrives at `/webhook/github` with an incorrect `X-Hub-Signature-256` value
- **THEN** the system responds with HTTP 401 Unauthorized
- **AND** does NOT process the event

### Requirement: Multi-repo event routing
The system SHALL determine the repository from the webhook payload, qualify it with the endpoint's forge, and route events to the correct repository's handler. The repo lookup SHALL be performed dynamically against the current managed repo set (which may change at runtime due to discovery). Webhooks for repositories not currently managed by this instance SHALL be ignored.

#### Scenario: Event for a managed repo
- **WHEN** a webhook arrives at `/webhook/github` for repo `org/app` and `github:org/app` is in the current managed set
- **THEN** the event is routed to `github:org/app`'s handler

#### Scenario: Event for an unmanaged repo
- **WHEN** a webhook arrives at `/webhook/gitea` for repo `org/other` which is NOT in the current managed set
- **THEN** the system responds with HTTP 200 but takes no action
- **AND** logs a debug message about the unrecognized repository

#### Scenario: Event for a recently discovered repo
- **WHEN** the discovery loop has just added `github:org/new-repo` to the managed set
- **AND** a webhook arrives at `/webhook/github` for `org/new-repo`
- **THEN** the event is routed to `github:org/new-repo`'s handler

#### Scenario: Event for a recently removed repo
- **WHEN** the discovery loop has just removed `gitea:org/old-repo` from the managed set
- **AND** a webhook arrives at `/webhook/gitea` for `org/old-repo`
- **THEN** the system responds with HTTP 200 but takes no action

## ADDED Requirements

### Requirement: Route GitHub pull_request events
The system SHALL parse GitHub webhook deliveries with `X-GitHub-Event: pull_request` and dispatch by `action`: `auto_merge_enabled` → enqueue; `auto_merge_disabled` → dequeue; `closed` → merged/closed handling; `synchronize` → new-push handling. Other actions SHALL be acknowledged with HTTP 200 and ignored.

#### Scenario: auto_merge_enabled
- **WHEN** `/webhook/github` receives `X-GitHub-Event: pull_request` with `action=auto_merge_enabled` for PR #42 in `org/app`
- **THEN** PR #42 is enqueued in `github:org/app`

#### Scenario: Unhandled action
- **WHEN** `/webhook/github` receives `X-GitHub-Event: pull_request` with `action=labeled`
- **THEN** the system responds 200 and takes no action

### Requirement: Route GitHub check_run and status events
The system SHALL parse GitHub webhook deliveries with `X-GitHub-Event: check_run` (action `completed`) and `X-GitHub-Event: status`, extract the commit SHA, context/check name, and conclusion/state, and route them to the check monitoring handler for the repo. Events whose context is `gitea-mq` SHALL be ignored.

#### Scenario: check_run completed on merge branch
- **WHEN** `/webhook/github` receives `check_run` with `action=completed`, name `ci/build`, conclusion `success`, on a SHA matching an active merge branch
- **THEN** the event is routed to the check monitoring handler with context `ci/build` and state `success`

#### Scenario: Own check run ignored
- **WHEN** `/webhook/github` receives `check_run` for check name `gitea-mq`
- **THEN** the system responds 200 and takes no action

### Requirement: Route GitHub installation events
The system SHALL parse GitHub webhook deliveries with `X-GitHub-Event: installation` or `installation_repositories` and trigger an immediate discovery refresh. These events SHALL NOT require a managed-repo lookup.

#### Scenario: installation_repositories added
- **WHEN** `/webhook/github` receives `installation_repositories` with `action=added`
- **THEN** the system responds 200
- **AND** triggers an immediate discovery run
