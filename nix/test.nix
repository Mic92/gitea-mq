# NixOS VM integration test: spins up Gitea + PostgreSQL + gitea-mq,
# verifies auto-setup configures branch protection and webhook,
# creates a PR with automerge, and checks the queue processes it.
{
  pkgs,
  self,
}:
pkgs.testers.runNixOSTest {
  name = "gitea-mq-integration";

  nodes.machine =
    {
      config,
      lib,
      pkgs,
      ...
    }:
    {
      imports = [ self.nixosModules.default ];

      # PostgreSQL for both Gitea and gitea-mq.
      services.postgresql = {
        enable = true;
        ensureDatabases = [
          "gitea"
          "gitea-mq"
        ];
        ensureUsers = [
          {
            name = "gitea";
            ensureDBOwnership = true;
          }
          {
            name = "gitea-mq";
            ensureDBOwnership = true;
          }
        ];
      };

      # Gitea instance.
      services.gitea = {
        enable = true;
        database = {
          type = "postgres";
          socket = "/run/postgresql";
          name = "gitea";
          user = "gitea";
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
        logLevel = "debug";
      };

      # Don't start gitea-mq until the test script has written the secrets.
      systemd.services.gitea-mq.wantedBy = lib.mkForce [ ];

      environment.systemPackages = [
        config.services.gitea.package
        pkgs.jq
      ];
    };

  testScript = ''
    import json

    machine.wait_for_unit("postgresql.service")
    machine.wait_for_unit("gitea.service")
    machine.wait_for_open_port(3000)

    # Create Gitea admin user via the gitea CLI.
    machine.succeed(
        "su -l gitea -c '"
        "GITEA_WORK_DIR=/var/lib/gitea gitea admin user create --admin --username testuser --password testpass123 --email test@test.com"
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
    # so auto-setup finds them on first run.
    machine.succeed(
        f"curl -sf -X POST http://localhost:3000/api/v1/user/repos "
        f"-H 'Authorization: token {token}' "
        f"-H 'Content-Type: application/json' "
        f"-d '{{\"name\": \"testrepo\", \"auto_init\": true, \"default_branch\": \"main\"}}'"
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

    # Health check.
    result = machine.succeed("curl -sf http://localhost:8080/healthz")
    assert result.strip() == "ok", f"health check failed: {result}"

    # Wait for auto-setup to add gitea-mq to branch protection.
    machine.wait_until_succeeds(
        f"curl -sf http://localhost:3000/api/v1/repos/testuser/testrepo/branch_protections/main "
        f"-H 'Authorization: token {token}' "
        f"| grep -q gitea-mq",
        timeout=30,
    )

    # Verify webhook was created.
    machine.wait_until_succeeds(
        f"curl -sf http://localhost:3000/api/v1/repos/testuser/testrepo/hooks "
        f"-H 'Authorization: token {token}' "
        f"| grep -q localhost:8080",
        timeout=30,
    )

    # Verify dashboard is serving.
    dash = machine.succeed("curl -sf http://localhost:8080/")
    assert "testuser/testrepo" in dash, f"dashboard missing repo: {dash}"

    # --- Test the actual merge queue flow ---

    # Create a feature branch with a file change.
    machine.succeed(
        f"curl -sf -X POST 'http://localhost:3000/api/v1/repos/testuser/testrepo/contents/test.txt' "
        f"-H 'Authorization: token {token}' "
        f"-H 'Content-Type: application/json' "
        f"-d '{{\"content\": \"dGVzdA==\", \"message\": \"add test file\", \"new_branch\": \"feature-1\"}}'"
    )

    # Create a PR from feature-1 → main.
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

    # Wait for the poller to detect automerge and enqueue the PR.
    # The dashboard repo page should show the PR.
    machine.wait_until_succeeds(
        f"curl -sf http://localhost:8080/repo/testuser/testrepo | grep -q 'PR #{pr_number}'",
        timeout=30,
    )

    # Wait for the poller to create the merge branch (state=testing).
    # The merge branch gitea-mq/<pr> appears in Gitea when the poller calls
    # StartTesting via git push.
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

    # The poller should have set gitea-mq to pending ("Testing merge result")
    # on the PR head.
    machine.wait_until_succeeds(
        f"curl -sf 'http://localhost:3000/api/v1/repos/testuser/testrepo/statuses/{pr_sha}' "
        f"-H 'Authorization: token {token}' "
        f"| jq -e '.[] | select(.context == \"gitea-mq\" and .status == \"pending\")'",
        timeout=30,
    )

    # --- Simulate external CI passing on the merge branch ---
    # Set ci/build=success on the merge branch SHA. Gitea will fire a
    # webhook to gitea-mq, whose monitor will then set gitea-mq=success
    # on the PR head.
    machine.succeed(
        f"curl -sf -X POST 'http://localhost:3000/api/v1/repos/testuser/testrepo/statuses/{merge_branch_sha}' "
        f"-H 'Authorization: token {token}' "
        f"-H 'Content-Type: application/json' "
        f"-d '{{\"context\": \"ci/build\", \"state\": \"success\", \"description\": \"build passed\"}}'"
    )

    # Wait for gitea-mq to set gitea-mq=success on the PR head via the
    # webhook → monitor flow.
    machine.wait_until_succeeds(
        f"curl -sf 'http://localhost:3000/api/v1/repos/testuser/testrepo/statuses/{pr_sha}' "
        f"-H 'Authorization: token {token}' "
        f"| jq -e '.[] | select(.context == \"gitea-mq\" and .status == \"success\")'",
        timeout=30,
    )

    # Also set ci/build=success on the PR head so Gitea's automerge sees
    # all required checks passing on the PR itself.
    machine.succeed(
        f"curl -sf -X POST 'http://localhost:3000/api/v1/repos/testuser/testrepo/statuses/{pr_sha}' "
        f"-H 'Authorization: token {token}' "
        f"-H 'Content-Type: application/json' "
        f"-d '{{\"context\": \"ci/build\", \"state\": \"success\", \"description\": \"build passed\"}}'"
    )

    # Wait for Gitea's automerge to merge the PR.
    machine.wait_until_succeeds(
        f"curl -sf 'http://localhost:3000/api/v1/repos/testuser/testrepo/pulls/{pr_number}' "
        f"-H 'Authorization: token {token}' "
        f"| jq -e '.merged == true'",
        timeout=60,
    )

    # After the merge, the poller should remove the PR from the queue.
    machine.wait_until_succeeds(
        f"! curl -sf http://localhost:8080/repo/testuser/testrepo "
        f"| grep -q 'PR #{pr_number}'",
        timeout=30,
    )

    # --- Topic-based discovery test ---
    # Create a second repo with the merge-queue topic and verify it gets discovered.
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
  '';
}
