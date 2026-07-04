{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.gitea-mq;
  giteaEnabled = cfg.giteaUrl != null;
  githubEnabled = cfg.github.appId != null;

  # Configure uploadpack.hideRefs in the forge's global git config so its
  # git upload-pack won't advertise gitea-mq/* merge branches to clients.
  mkHideRefsPre =
    stateDir:
    let
      hideRef = "refs/heads/gitea-mq/";
    in
    lib.mkAfter [
      (pkgs.writeShellScript "gitea-mq-hide-refs" ''
        export HOME=${lib.escapeShellArg stateDir}
        export GIT_CONFIG_NOSYSTEM=1
        # Add hideRefs if not already present (idempotent).
        if ! ${pkgs.git}/bin/git config --global --get uploadpack.hideRefs '^${hideRef}$' >/dev/null 2>&1; then
          ${pkgs.git}/bin/git config --global --add uploadpack.hideRefs ${hideRef}
        fi
      '')
    ];
in
{
  options.services.gitea-mq = {
    enable = lib.mkEnableOption "gitea-mq merge queue service";

    package = lib.mkOption {
      type = lib.types.package;
      default = pkgs.callPackage ./package.nix { };
      defaultText = lib.literalExpression "pkgs.callPackage ./package.nix { }";
      description = "The gitea-mq package to use.";
    };

    giteaUrl = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Gitea instance URL. Setting this enables the Gitea backend.";
      example = "https://gitea.example.com";
    };

    giteaTokenFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "Path to a file containing the Gitea API token.";
    };

    github = {
      appId = lib.mkOption {
        type = lib.types.nullOr lib.types.int;
        default = null;
        description = "GitHub App ID. Setting this enables the GitHub backend.";
      };
      privateKeyFile = lib.mkOption {
        type = lib.types.nullOr lib.types.path;
        default = null;
        description = "Path to the GitHub App private key (PEM).";
      };
      webhookSecretFile = lib.mkOption {
        type = lib.types.nullOr lib.types.path;
        default = null;
        description = "Path to a file containing the GitHub App webhook secret.";
      };
      repos = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [ ];
        description = "GitHub repos to manage in addition to all repos the App is installed on.";
      };
      pollInterval = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        description = "Override the reconcile poll interval for GitHub. Defaults to `pollInterval`.";
      };
    };

    repos = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = "List of repos to manage in owner/name format. Optional when topic is set.";
      example = [
        "org/app"
        "org/lib"
      ];
    };

    topic = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Gitea topic to discover repos by. Repos with this topic and admin access will be managed automatically.";
      example = "merge-queue";
    };

    databaseUrl = lib.mkOption {
      type = lib.types.str;
      default = "postgres:///gitea-mq?host=/run/postgresql";
      description = "PostgreSQL connection string.";
    };

    webhookSecretFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "Path to a file containing the Gitea webhook HMAC secret.";
    };

    listenAddr = lib.mkOption {
      type = lib.types.str;
      default = ":8080";
      description = "HTTP listen address.";
    };

    webhookPath = lib.mkOption {
      type = lib.types.str;
      default = "/webhook";
      description = "Webhook endpoint path.";
    };

    externalUrl = lib.mkOption {
      type = lib.types.str;
      description = "External URL where Gitea can reach this service (for webhook auto-setup).";
      example = "https://mq.example.com";
    };

    pollInterval = lib.mkOption {
      type = lib.types.str;
      default = "30s";
      description = "Automerge discovery poll interval.";
    };

    checkTimeout = lib.mkOption {
      type = lib.types.str;
      default = "1h";
      description = "Timeout for required checks.";
    };

    skipQueueIfUpToDate = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = "Skip the merge-branch CI run for PRs already rebased onto the target branch tip.";
    };

    requiredChecks = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = "Fallback required check contexts (if branch protection doesn't specify them).";
    };

    batchMax = lib.mkOption {
      type = lib.types.int;
      default = 1;
      description = ''
        Maximum number of queued PRs tested together as one bors-style batch.
        1 disables batching (legacy single-PR path); 0 batches everything
        currently queued. Batched PRs land as merge commits, so leave at 1 for
        repos that mandate squash/rebase merges.
      '';
    };

    bisectMaxSteps = lib.mkOption {
      type = lib.types.int;
      default = 0;
      description = "Cap on CI builds spent bisecting one failing batch. 0 means unlimited.";
    };

    refreshInterval = lib.mkOption {
      type = lib.types.str;
      default = "10s";
      description = "Dashboard auto-refresh interval.";
    };

    discoveryInterval = lib.mkOption {
      type = lib.types.str;
      default = "5m";
      description = "How often to re-discover repos by topic. Only used when topic is set.";
    };

    logLevel = lib.mkOption {
      type = lib.types.enum [
        "debug"
        "info"
        "warn"
        "error"
      ];
      default = "info";
      description = "Log level.";
    };

    hideRefFromClients = lib.mkOption {
      type = lib.types.bool;
      default = config.services.gitea.enable || config.services.forgejo.enable;
      defaultText = lib.literalExpression "config.services.gitea.enable || config.services.forgejo.enable";
      description = ''
        Hide gitea-mq/* merge branches from git client fetches by setting
        `uploadpack.hideRefs` in the forge's global git config.

        This prevents git clients from downloading temporary merge queue
        branches during `git fetch`. Only effective when Gitea/Forgejo runs
        on the same host. Enabled automatically when `services.gitea` or
        `services.forgejo` is enabled.

        For non-NixOS deployments, run manually:
          git config --global uploadpack.hideRefs refs/heads/gitea-mq/
        as the Gitea system user.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = giteaEnabled || githubEnabled;
        message = "services.gitea-mq: configure at least one backend (giteaUrl or github.appId).";
      }
      {
        assertion = !giteaEnabled || (cfg.giteaTokenFile != null && cfg.webhookSecretFile != null);
        message = "services.gitea-mq: giteaTokenFile and webhookSecretFile are required when giteaUrl is set.";
      }
      {
        assertion =
          !githubEnabled || (cfg.github.privateKeyFile != null && cfg.github.webhookSecretFile != null);
        message = "services.gitea-mq: github.privateKeyFile and github.webhookSecretFile are required when github.appId is set.";
      }
    ];

    # Hide gitea-mq/* branches from git fetch by configuring uploadpack.hideRefs
    # in the forge's global git config. This runs as the forge user before the
    # forge starts, so that its git upload-pack won't advertise these refs.
    systemd.services.gitea = lib.mkIf (cfg.hideRefFromClients && config.services.gitea.enable) {
      serviceConfig.ExecStartPre = mkHideRefsPre config.services.gitea.stateDir;
    };
    systemd.services.forgejo = lib.mkIf (cfg.hideRefFromClients && config.services.forgejo.enable) {
      serviceConfig.ExecStartPre = mkHideRefsPre config.services.forgejo.stateDir;
    };

    systemd.services.gitea-mq = {
      description = "gitea-mq merge queue for Gitea";
      after = [
        "network.target"
        "postgresql.service"
      ];
      wants = [ "postgresql.service" ];
      wantedBy = [ "multi-user.target" ];

      serviceConfig = {
        Type = "simple";
        DynamicUser = true;
        Restart = "on-failure";
        RestartSec = 5;

        # Credentials: systemd loads files and exposes them under /run/credentials.
        LoadCredential =
          lib.optionals giteaEnabled [
            "gitea-token:${cfg.giteaTokenFile}"
            "webhook-secret:${cfg.webhookSecretFile}"
          ]
          ++ lib.optionals githubEnabled [
            "github-private-key:${cfg.github.privateKeyFile}"
            "github-webhook-secret:${cfg.github.webhookSecretFile}"
          ];
      };

      environment = {
        GITEA_MQ_DATABASE_URL = cfg.databaseUrl;
        GITEA_MQ_LISTEN_ADDR = cfg.listenAddr;
        GITEA_MQ_WEBHOOK_PATH = cfg.webhookPath;
        GITEA_MQ_EXTERNAL_URL = cfg.externalUrl;
        GITEA_MQ_POLL_INTERVAL = cfg.pollInterval;
        GITEA_MQ_CHECK_TIMEOUT = cfg.checkTimeout;
        GITEA_MQ_SKIP_QUEUE_IF_UP_TO_DATE = lib.boolToString cfg.skipQueueIfUpToDate;
        GITEA_MQ_BATCH_MAX = toString cfg.batchMax;
        GITEA_MQ_BISECT_MAX_STEPS = toString cfg.bisectMaxSteps;
        GITEA_MQ_REFRESH_INTERVAL = cfg.refreshInterval;
        GITEA_MQ_DISCOVERY_INTERVAL = cfg.discoveryInterval;
        GITEA_MQ_LOG_LEVEL = cfg.logLevel;
      }
      // lib.optionalAttrs (cfg.repos != [ ]) {
        GITEA_MQ_REPOS = lib.concatStringsSep "," cfg.repos;
      }
      // lib.optionalAttrs (cfg.topic != null) {
        GITEA_MQ_TOPIC = cfg.topic;
      }
      // lib.optionalAttrs (cfg.requiredChecks != [ ]) {
        GITEA_MQ_REQUIRED_CHECKS = lib.concatStringsSep "," cfg.requiredChecks;
      }
      // lib.optionalAttrs giteaEnabled {
        GITEA_MQ_GITEA_URL = cfg.giteaUrl;
      }
      // lib.optionalAttrs githubEnabled (
        {
          GITEA_MQ_GITHUB_APP_ID = toString cfg.github.appId;
        }
        // lib.optionalAttrs (cfg.github.repos != [ ]) {
          GITEA_MQ_GITHUB_REPOS = lib.concatStringsSep "," cfg.github.repos;
        }
        // lib.optionalAttrs (cfg.github.pollInterval != null) {
          GITEA_MQ_GITHUB_POLL_INTERVAL = cfg.github.pollInterval;
        }
      );

      path = [ pkgs.git ];

      # Script wrapper to load secrets from credential files into env vars.
      script = ''
        ${lib.optionalString giteaEnabled ''
          export GITEA_MQ_GITEA_TOKEN="$(< "$CREDENTIALS_DIRECTORY/gitea-token")"
          export GITEA_MQ_WEBHOOK_SECRET="$(< "$CREDENTIALS_DIRECTORY/webhook-secret")"
        ''}
        ${lib.optionalString githubEnabled ''
          export GITEA_MQ_GITHUB_PRIVATE_KEY_FILE="$CREDENTIALS_DIRECTORY/github-private-key"
          export GITEA_MQ_GITHUB_WEBHOOK_SECRET="$(< "$CREDENTIALS_DIRECTORY/github-webhook-secret")"
        ''}
        exec ${lib.getExe cfg.package}
      '';
    };
  };
}
