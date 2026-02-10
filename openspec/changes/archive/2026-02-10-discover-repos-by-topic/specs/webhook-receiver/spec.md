## MODIFIED Requirements

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
