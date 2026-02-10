## ADDED Requirements

### Requirement: HTTP webhook endpoint
The system SHALL expose an HTTP endpoint that accepts POST requests from Gitea webhooks. The endpoint path SHALL be configurable, defaulting to `/webhook`.

#### Scenario: Valid webhook POST
- **WHEN** Gitea sends a POST request to `/webhook` with a valid event payload
- **THEN** the system responds with HTTP 200
- **AND** processes the event asynchronously

#### Scenario: Non-POST request
- **WHEN** a GET request is sent to `/webhook`
- **THEN** the system responds with HTTP 405 Method Not Allowed

### Requirement: HMAC signature validation
The system SHALL validate the `X-Gitea-Signature` header on every webhook request using the configured shared secret. Requests with missing or invalid signatures SHALL be rejected.

#### Scenario: Valid signature
- **WHEN** a webhook request arrives with a valid HMAC-SHA256 signature in `X-Gitea-Signature`
- **THEN** the system accepts and processes the request

#### Scenario: Missing signature header
- **WHEN** a webhook request arrives without an `X-Gitea-Signature` header
- **THEN** the system responds with HTTP 401 Unauthorized
- **AND** does NOT process the event

#### Scenario: Invalid signature
- **WHEN** a webhook request arrives with an incorrect `X-Gitea-Signature` value
- **THEN** the system responds with HTTP 401 Unauthorized
- **AND** does NOT process the event

### Requirement: Ignore own status events
The system SHALL ignore commit status webhook events where the context is `gitea-mq`. This prevents a feedback loop where posting a `gitea-mq` status triggers a webhook that causes the system to re-process its own status update.

#### Scenario: Webhook for gitea-mq status
- **WHEN** a commit status webhook arrives with context `gitea-mq`
- **THEN** the system responds with HTTP 200 but takes no action

### Requirement: Idempotent event handling
The system SHALL handle duplicate webhook deliveries idempotently. Processing the same event multiple times SHALL have no additional effect beyond the first processing.

#### Scenario: Duplicate status event
- **WHEN** a commit status event for `ci/build` = `success` on merge branch `mq/42` is delivered twice
- **THEN** the second delivery has no additional effect — the check is already recorded as `success`

### Requirement: Route commit_status events
The system SHALL parse commit status webhook events and route them to the check monitoring handler. The system SHALL extract the commit SHA, status context name, state, and repository from the payload.

#### Scenario: Commit status update for merge branch
- **WHEN** a webhook with a commit status event arrives for a commit on a merge queue branch
- **THEN** the system routes it to the check monitoring handler with the commit SHA, context name, and state

#### Scenario: Commit status update for non-merge-queue branch
- **WHEN** a webhook with a commit status event arrives for a commit not on any merge queue branch
- **THEN** the system ignores the event (responds 200 but takes no action)

### Requirement: Multi-repo event routing
The system SHALL determine the repository from the webhook payload and route events to the correct repository's queue. The repo lookup SHALL be performed dynamically against the current managed repo set (which may change at runtime due to topic-based discovery). Webhooks for repositories not currently managed by this instance SHALL be ignored.

#### Scenario: Event for a managed repo
- **WHEN** a webhook arrives for repo `org/app` which is in the current managed set
- **THEN** the event is routed to `org/app`'s check monitoring handler

#### Scenario: Event for an unmanaged repo
- **WHEN** a webhook arrives for repo `org/other` which is NOT in the current managed set
- **THEN** the system responds with HTTP 200 but takes no action
- **AND** logs a debug message about the unrecognized repository

#### Scenario: Event for a recently discovered repo
- **WHEN** the discovery loop has just added `org/new-repo` to the managed set
- **AND** a webhook arrives for `org/new-repo`
- **THEN** the event is routed to `org/new-repo`'s check monitoring handler

#### Scenario: Event for a recently removed repo
- **WHEN** the discovery loop has just removed `org/old-repo` from the managed set
- **AND** a webhook arrives for `org/old-repo`
- **THEN** the system responds with HTTP 200 but takes no action

### Requirement: Malformed payload handling
The system SHALL gracefully handle malformed or unparseable webhook payloads without crashing.

#### Scenario: Invalid JSON payload
- **WHEN** a webhook request arrives with a valid signature but the body is not valid JSON
- **THEN** the system responds with HTTP 400 Bad Request
- **AND** logs the error

#### Scenario: Missing required fields
- **WHEN** a webhook request arrives with valid JSON but is missing required fields (e.g., no repository info)
- **THEN** the system responds with HTTP 400 Bad Request
- **AND** logs the error
