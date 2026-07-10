# NixOS VM integration test: spins up a forge (Gitea or Forgejo) +
# PostgreSQL + gitea-mq, verifies auto-setup configures branch protection
# and webhook, creates a PR with automerge, and checks the queue processes it.
#
# The same test script runs against both forges. Forgejo shares Gitea's
# REST API and architecture, so only the NixOS module wiring (service name,
# state dir, CLI binary, system user) differs.
{
  pkgs,
  self,
  # Which forge to test: "gitea" or "forgejo".
  forge ? "gitea",
}:
let
  forgeParams = {
    gitea = {
      module = "services.gitea";
      service = "gitea.service";
      user = "gitea";
      stateDir = "/var/lib/gitea";
      cli = "gitea";
      workDirEnv = "GITEA_WORK_DIR";
    };
    forgejo = {
      module = "services.forgejo";
      service = "forgejo.service";
      user = "forgejo";
      stateDir = "/var/lib/forgejo";
      cli = "forgejo";
      workDirEnv = "FORGEJO_WORK_DIR";
    };
  };
  p = forgeParams.${forge};
in
pkgs.testers.runNixOSTest {
  name = "gitea-mq-integration-${forge}";

  nodes.machine =
    {
      config,
      lib,
      pkgs,
      ...
    }:
    {
      imports = [ self.nixosModules.default ];

      # PostgreSQL for both the forge and gitea-mq.
      services.postgresql = {
        enable = true;
        ensureDatabases = [
          p.user
          "gitea-mq"
        ];
        ensureUsers = [
          {
            name = p.user;
            ensureDBOwnership = true;
          }
          {
            name = "gitea-mq";
            ensureDBOwnership = true;
          }
        ];
      };

      # Forge instance (Gitea or Forgejo). Both modules share option names.
      services.${forge} = {
        enable = true;
        database = {
          type = "postgres";
          socket = "/run/postgresql";
          name = p.user;
          user = p.user;
        };
        settings = {
          server = {
            ROOT_URL = "http://localhost:3000";
            HTTP_PORT = 3000;
          };
          service.DISABLE_REGISTRATION = false;
          webhook = {
            ALLOWED_HOST_LIST = "loopback";
          };
        };
      };

      # Secrets written by the test script before starting gitea-mq.
      systemd.tmpfiles.rules = [
        "d /run/gitea-mq 0755 root root -"
      ];

      services.gitea-mq = {
        enable = true;
        package = self.packages.${pkgs.stdenv.hostPlatform.system}.gitea-mq;
        giteaUrl = "http://localhost:3000";
        giteaTokenFile = "/run/gitea-mq/token";
        repos = [ "testuser/testrepo" ];
        topic = "merge-queue";
        databaseUrl = "postgres:///gitea-mq?host=/run/postgresql";
        webhookSecretFile = "/run/gitea-mq/secret";
        listenAddr = ":8080";
        externalUrl = "http://localhost:8080";
        pollInterval = "5s";
        discoveryInterval = "5s";
        checkTimeout = "5m";
        # The test asserts the full merge-branch flow; the only PR it opens is
        # already up-to-date with main, so the default skip would bypass it.
        skipQueueIfUpToDate = false;
        logLevel = "debug";
      };

      # Don't start gitea-mq until the test script has written the secrets.
      systemd.services.gitea-mq.wantedBy = lib.mkForce [ ];

      environment.systemPackages = [
        config.services.${forge}.package
        pkgs.jq
      ];
    };

  testScript = ''
    import json

    machine.wait_for_unit("postgresql.service")
    machine.wait_for_unit("${p.service}")
    machine.wait_for_open_port(3000)

    # Create the forge admin user via its CLI.
    machine.succeed(
        "su -l ${p.user} -c '"
        "${p.workDirEnv}=${p.stateDir} ${p.cli} admin user create --admin --username testuser --password testpass123 --email test@test.com"
        "'"
    )

    # Create API token.
    token_json = machine.succeed(
        "curl -sf -X POST http://localhost:3000/api/v1/users/testuser/tokens "
        "-u testuser:testpass123 "
        "-H 'Content-Type: application/json' "
        "-d '{\"name\": \"mq-token\", \"scopes\": [\"all\"]}'"
    )
    token = json.loads(token_json)["sha1"]

    # Create a repo and branch protection before starting gitea-mq,
    # so auto-setup finds them on first run. The repo is private so the merge
    # queue's git operations (fetch, lazy blob fetch, push) must authenticate.
    machine.succeed(
        f"curl -sf -X POST http://localhost:3000/api/v1/user/repos "
        f"-H 'Authorization: token {token}' "
        f"-H 'Content-Type: application/json' "
        f"-d '{{\"name\": \"testrepo\", \"auto_init\": true, \"default_branch\": \"main\", \"private\": true}}'"
    )

    # Create a file with many lines up front (before branch protection blocks
    # direct pushes to main). Used later by the long-timeline regression test.
    import base64
    big_content = base64.b64encode(
        "\n".join(f"line {i}" for i in range(1, 101)).encode()
    ).decode()
    machine.succeed(
        f"curl -sf -X POST 'http://localhost:3000/api/v1/repos/testuser/testrepo/contents/big.txt' "
        f"-H 'Authorization: token {token}' "
        f"-H 'Content-Type: application/json' "
        f"-d '{{\"content\": \"{big_content}\", \"message\": \"add big file\"}}'"
    )

    machine.succeed(
        f"curl -sf -X POST http://localhost:3000/api/v1/repos/testuser/testrepo/branch_protections "
        f"-H 'Authorization: token {token}' "
        f"-H 'Content-Type: application/json' "
        f"-d '{{\"branch_name\": \"main\", \"enable_status_check\": true, \"status_check_contexts\": [\"ci/build\"]}}'"
    )

    # Write secrets and start gitea-mq.
    machine.succeed(f"echo -n '{token}' > /run/gitea-mq/token")
    machine.succeed("echo -n 'test-webhook-secret' > /run/gitea-mq/secret")
    machine.systemctl("start gitea-mq.service")

    machine.wait_for_open_port(8080)

    result = machine.succeed("curl -sf http://localhost:8080/healthz")
    assert result.strip() == "ok", f"health check failed: {result}"

    # auto-setup adds gitea-mq to the branch protection required checks.
    machine.wait_until_succeeds(
        f"curl -sf http://localhost:3000/api/v1/repos/testuser/testrepo/branch_protections/main "
        f"-H 'Authorization: token {token}' "
        f"| grep -q gitea-mq",
        timeout=30,
    )

    machine.wait_until_succeeds(
        f"curl -sf http://localhost:3000/api/v1/repos/testuser/testrepo/hooks "
        f"-H 'Authorization: token {token}' "
        f"| grep -q localhost:8080",
        timeout=30,
    )

    dash = machine.succeed("curl -sf http://localhost:8080/")
    assert "testuser/testrepo" in dash, f"dashboard missing repo: {dash}"

    # --- Merge queue happy path ---

    machine.succeed(
        f"curl -sf -X POST 'http://localhost:3000/api/v1/repos/testuser/testrepo/contents/test.txt' "
        f"-H 'Authorization: token {token}' "
        f"-H 'Content-Type: application/json' "
        f"-d '{{\"content\": \"dGVzdA==\", \"message\": \"add test file\", \"new_branch\": \"feature-1\"}}'"
    )

    pr_json = machine.succeed(
        f"curl -sf -X POST 'http://localhost:3000/api/v1/repos/testuser/testrepo/pulls' "
        f"-H 'Authorization: token {token}' "
        f"-H 'Content-Type: application/json' "
        f"-d '{{\"title\": \"Test PR\", \"head\": \"feature-1\", \"base\": \"main\"}}'"
    )
    pr = json.loads(pr_json)
    pr_number = pr["number"]
    pr_sha = pr["head"]["sha"]

    # Schedule automerge ("merge when checks succeed").
    machine.succeed(
        f"curl -sf -X POST 'http://localhost:3000/api/v1/repos/testuser/testrepo/pulls/{pr_number}/merge' "
        f"-H 'Authorization: token {token}' "
        f"-H 'Content-Type: application/json' "
        f"-d '{{\"Do\": \"merge\", \"merge_when_checks_succeed\": true}}'"
    )

    # Set ci/build=success on the PR head so the poller's CI gate allows enqueue.
    machine.succeed(
        f"curl -sf -X POST 'http://localhost:3000/api/v1/repos/testuser/testrepo/statuses/{pr_sha}' "
        f"-H 'Authorization: token {token}' "
        f"-H 'Content-Type: application/json' "
        f"-d '{{\"context\": \"ci/build\", \"state\": \"success\", \"description\": \"build passed\"}}'"
    )

    # Poller detects automerge and enqueues the PR.
    machine.wait_until_succeeds(
        f"curl -sf http://localhost:8080/repo/testuser/testrepo | grep -q 'PR #{pr_number}'",
        timeout=30,
    )

    # Poller pushes the merge branch gitea-mq/<pr> (state=testing).
    merge_branch_sha = ""
    machine.wait_until_succeeds(
        f"curl -sf 'http://localhost:3000/api/v1/repos/testuser/testrepo/branches/gitea-mq/{pr_number}' "
        f"-H 'Authorization: token {token}' "
        f"| jq -e '.commit.id'",
        timeout=30,
    )
    merge_branch_json = machine.succeed(
        f"curl -sf 'http://localhost:3000/api/v1/repos/testuser/testrepo/branches/gitea-mq/{pr_number}' "
        f"-H 'Authorization: token {token}'"
    )
    merge_branch_sha = json.loads(merge_branch_json)["commit"]["id"]

    # gitea-mq status goes pending ("Testing merge result") on the PR head.
    machine.wait_until_succeeds(
        f"curl -sf 'http://localhost:3000/api/v1/repos/testuser/testrepo/statuses/{pr_sha}' "
        f"-H 'Authorization: token {token}' "
        f"| jq -e '.[] | select(.context == \"gitea-mq\" and .status == \"pending\")'",
        timeout=30,
    )

    # External CI passes on the merge branch. gitea-mq picks this up via the
    # commit-status webhook (Gitea) or the poller's status poll (Forgejo, which
    # has no status webhook) and sets gitea-mq=success on the PR head.
    machine.succeed(
        f"curl -sf -X POST 'http://localhost:3000/api/v1/repos/testuser/testrepo/statuses/{merge_branch_sha}' "
        f"-H 'Authorization: token {token}' "
        f"-H 'Content-Type: application/json' "
        f"-d '{{\"context\": \"ci/build\", \"state\": \"success\", \"description\": \"build passed\"}}'"
    )

    machine.wait_until_succeeds(
        f"curl -sf 'http://localhost:3000/api/v1/repos/testuser/testrepo/statuses/{pr_sha}' "
        f"-H 'Authorization: token {token}' "
        f"| jq -e '.[] | select(.context == \"gitea-mq\" and .status == \"success\")'",
        timeout=30,
    )

    # Merge branch CI status is mirrored to the PR head under a prefixed context.
    machine.wait_until_succeeds(
        f"curl -sf 'http://localhost:3000/api/v1/repos/testuser/testrepo/statuses/{pr_sha}' "
        f"-H 'Authorization: token {token}' "
        f"| jq -e '.[] | select(.context == \"gitea-mq/ci/build\" and .status == \"success\" and .description == \"build passed\")'",
        timeout=30,
    )

    # ci/build=success on the PR head too, so the forge's automerge sees all
    # required checks passing on the PR itself.
    machine.succeed(
        f"curl -sf -X POST 'http://localhost:3000/api/v1/repos/testuser/testrepo/statuses/{pr_sha}' "
        f"-H 'Authorization: token {token}' "
        f"-H 'Content-Type: application/json' "
        f"-d '{{\"context\": \"ci/build\", \"state\": \"success\", \"description\": \"build passed\"}}'"
    )

    # Forge automerge merges the PR.
    machine.wait_until_succeeds(
        f"curl -sf 'http://localhost:3000/api/v1/repos/testuser/testrepo/pulls/{pr_number}' "
        f"-H 'Authorization: token {token}' "
        f"| jq -e '.merged == true'",
        timeout=60,
    )

    # Poller removes the merged PR from the queue.
    machine.wait_until_succeeds(
        f"! curl -sf http://localhost:8080/repo/testuser/testrepo "
        f"| grep -q 'PR #{pr_number}'",
        timeout=30,
    )

    # --- Verify uploadpack.hideRefs is configured ---
    machine.succeed(
        "su -l ${p.user} -s /bin/sh -c '"
        "export HOME=${p.stateDir} GIT_CONFIG_NOSYSTEM=1; "
        "${pkgs.git}/bin/git config --global --get uploadpack.hideRefs | grep -q refs/heads/gitea-mq/"
        "'"
    )

    # --- Regression: long timeline with filtered events ---
    # The timeline API filters CommentTypeCode entries (inline code review
    # comments) AFTER applying the SQL LIMIT, so a page can return fewer items
    # than the limit even when more pages exist. A paginator that stops on
    # len(items) < limit would miss later pages holding the automerge event.
    # Bury the automerge event behind many inline comments and confirm the
    # poller still finds it.
    modified_content = base64.b64encode(
        "\n".join(f"modified line {i}" for i in range(1, 101)).encode()
    ).decode()
    file_info = json.loads(machine.succeed(
        f"curl -sf 'http://localhost:3000/api/v1/repos/testuser/testrepo/contents/big.txt' "
        f"-H 'Authorization: token {token}'"
    ))
    file_sha = file_info["sha"]
    machine.succeed(
        f"curl -sf -X PUT 'http://localhost:3000/api/v1/repos/testuser/testrepo/contents/big.txt' "
        f"-H 'Authorization: token {token}' "
        f"-H 'Content-Type: application/json' "
        f"-d '{{\"content\": \"{modified_content}\", \"message\": \"modify big file\", \"sha\": \"{file_sha}\", \"new_branch\": \"long-timeline\"}}'"
    )

    # Create PR from long-timeline → main.
    pr2_json = machine.succeed(
        f"curl -sf -X POST 'http://localhost:3000/api/v1/repos/testuser/testrepo/pulls' "
        f"-H 'Authorization: token {token}' "
        f"-H 'Content-Type: application/json' "
        f"-d '{{\"title\": \"Long timeline PR\", \"head\": \"long-timeline\", \"base\": \"main\"}}'"
    )
    pr2 = json.loads(pr2_json)
    pr2_number = pr2["number"]
    pr2_sha = pr2["head"]["sha"]

    # Submit a code review with many inline comments to generate
    # CommentTypeCode timeline entries that Gitea filters from the API
    # response (producing short pages in the paginated timeline).
    review_comments = [
        {
            "path": "big.txt",
            "body": f"review comment on line {i}",
            "new_position": i,
            "old_position": 0,
        }
        for i in range(1, 61)
    ]
    import json as json_mod
    review_body = json_mod.dumps({
        "event": "COMMENT",
        "body": "Code review with many inline comments",
        "commit_id": pr2_sha,
        "comments": review_comments,
    })
    machine.succeed(
        f"curl -sf -X POST 'http://localhost:3000/api/v1/repos/testuser/testrepo/pulls/{pr2_number}/reviews' "
        f"-H 'Authorization: token {token}' "
        f"-H 'Content-Type: application/json' "
        f"-d '{review_body}'"
    )

    # Verify the timeline has many entries but the paginated first page
    # returns fewer than 50 (confirming the filtering is active).
    unpaginated = json.loads(machine.succeed(
        f"curl -sf 'http://localhost:3000/api/v1/repos/testuser/testrepo/issues/{pr2_number}/timeline' "
        f"-H 'Authorization: token {token}'"
    ))
    page1 = json.loads(machine.succeed(
        f"curl -sf 'http://localhost:3000/api/v1/repos/testuser/testrepo/issues/{pr2_number}/timeline?page=1&limit=50' "
        f"-H 'Authorization: token {token}'"
    ))
    assert len(page1) < 50, (
        f"expected page 1 to have fewer than 50 items due to CommentTypeCode "
        f"filtering, got {len(page1)} (total unpaginated: {len(unpaginated)})"
    )

    # Schedule automerge now: the event lands at the end of the timeline,
    # past the filtered code review entries.
    machine.succeed(
        f"curl -sf -X POST 'http://localhost:3000/api/v1/repos/testuser/testrepo/pulls/{pr2_number}/merge' "
        f"-H 'Authorization: token {token}' "
        f"-H 'Content-Type: application/json' "
        f"-d '{{\"Do\": \"merge\", \"merge_when_checks_succeed\": true}}'"
    )

    # Set ci/build=success on PR head so the poller's CI gate allows enqueue.
    machine.succeed(
        f"curl -sf -X POST 'http://localhost:3000/api/v1/repos/testuser/testrepo/statuses/{pr2_sha}' "
        f"-H 'Authorization: token {token}' "
        f"-H 'Content-Type: application/json' "
        f"-d '{{\"context\": \"ci/build\", \"state\": \"success\", \"description\": \"build passed\"}}'"
    )

    # Poller finds the automerge event despite it being on a later page.
    machine.wait_until_succeeds(
        f"curl -sf http://localhost:8080/repo/testuser/testrepo | grep -q 'PR #{pr2_number}'",
        timeout=30,
    )

    # --- Topic-based discovery ---
    machine.succeed(
        f"curl -sf -X POST http://localhost:3000/api/v1/user/repos "
        f"-H 'Authorization: token {token}' "
        f"-H 'Content-Type: application/json' "
        f"-d '{{\"name\": \"discovered-repo\", \"auto_init\": true, \"default_branch\": \"main\"}}'"
    )
    machine.succeed(
        f"curl -sf -X PUT 'http://localhost:3000/api/v1/repos/testuser/discovered-repo/topics/merge-queue' "
        f"-H 'Authorization: token {token}'"
    )

    # Wait for the discovery loop to pick it up and show it on the dashboard.
    machine.wait_until_succeeds(
        "curl -sf http://localhost:8080/ | grep -q 'testuser/discovered-repo'",
        timeout=30,
    )

    # As a site admin, gitea-mq discovers topic-tagged repos even when it is
    # not a collaborator, including repos owned by other users.
    machine.succeed(
        "su -l ${p.user} -c '"
        "${p.workDirEnv}=${p.stateDir} ${p.cli} admin user create --username otheruser --password otherpass123 --email other@test.com"
        "'"
    )

    # Admin creates a repo under otheruser via the admin API.
    machine.succeed(
        f"curl -sf -X POST http://localhost:3000/api/v1/admin/users/otheruser/repos "
        f"-H 'Authorization: token {token}' "
        f"-H 'Content-Type: application/json' "
        f"-d '{{\"name\": \"other-repo\", \"auto_init\": true, \"default_branch\": \"main\"}}'"
    )
    machine.succeed(
        f"curl -sf -X PUT 'http://localhost:3000/api/v1/repos/otheruser/other-repo/topics/merge-queue' "
        f"-H 'Authorization: token {token}'"
    )

    # The admin's gitea-mq should discover otheruser/other-repo via the topic.
    machine.wait_until_succeeds(
        "curl -sf http://localhost:8080/ | grep -q 'otheruser/other-repo'",
        timeout=60,
    )
  '';
}
