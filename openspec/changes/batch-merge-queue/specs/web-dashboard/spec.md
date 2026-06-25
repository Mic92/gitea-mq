## MODIFIED Requirements

### Requirement: Repository queue detail page
When a live batch exists for a target branch, the page SHALL render its members grouped under a batch header showing batch id, branch name (linked to forge), `builds` count, and `len(current)/len(members)`. Member rows SHALL be tagged `current`, `pending`, or `landed`.

#### Scenario: Active batch displayed
- **WHEN** batch #7 has `members=[#10,#20,#30]`, `current=[#10,#20,#30]`, `builds=1`, `state=testing`
- **THEN** the repo page shows "Batch #7 · gitea-mq/batch/7 · build 1 · testing 3/3" with rows for #10,#20,#30 tagged `current`

#### Scenario: Bisection displayed
- **WHEN** batch #7 has `current=[#10,#20]`, `pending=[[#30,#40]]`, `landed=[]`, `builds=2`
- **THEN** the header shows "build 2 · testing 2/4" and #30,#40 rows are tagged `pending`
