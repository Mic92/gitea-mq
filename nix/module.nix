{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.gitea-mq;
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
      type = lib.types.str;
      description = "Gitea instance URL.";
      example = "https://gitea.example.com";
    };

    giteaTokenFile = lib.mkOption {
      type = lib.types.path;
      description = "Path to a file containing the Gitea API token.";
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
      type = lib.types.path;
      description = "Path to a file containing the webhook HMAC secret.";
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

    requiredChecks = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = "Fallback required check contexts (if branch protection doesn't specify them).";
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
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.topic != null || cfg.repos != [ ];
        message = "services.gitea-mq: at least one of 'topic' or 'repos' must be set.";
      }
    ];

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
        LoadCredential = [
          "gitea-token:${cfg.giteaTokenFile}"
          "webhook-secret:${cfg.webhookSecretFile}"
        ];
      };

      environment = {
        GITEA_MQ_GITEA_URL = cfg.giteaUrl;
        GITEA_MQ_DATABASE_URL = cfg.databaseUrl;
        GITEA_MQ_LISTEN_ADDR = cfg.listenAddr;
        GITEA_MQ_WEBHOOK_PATH = cfg.webhookPath;
        GITEA_MQ_EXTERNAL_URL = cfg.externalUrl;
        GITEA_MQ_POLL_INTERVAL = cfg.pollInterval;
        GITEA_MQ_CHECK_TIMEOUT = cfg.checkTimeout;
        GITEA_MQ_REFRESH_INTERVAL = cfg.refreshInterval;
        GITEA_MQ_LOG_LEVEL = cfg.logLevel;
      }
      // lib.optionalAttrs (cfg.repos != [ ]) {
        GITEA_MQ_REPOS = lib.concatStringsSep "," cfg.repos;
      }
      // lib.optionalAttrs (cfg.topic != null) {
        GITEA_MQ_TOPIC = cfg.topic;
        GITEA_MQ_DISCOVERY_INTERVAL = cfg.discoveryInterval;
      }
      // lib.optionalAttrs (cfg.requiredChecks != [ ]) {
        GITEA_MQ_REQUIRED_CHECKS = lib.concatStringsSep "," cfg.requiredChecks;
      };

      path = [ pkgs.git ];

      # Script wrapper to load secrets from credential files into env vars.
      script = ''
        export GITEA_MQ_GITEA_TOKEN="$(< "$CREDENTIALS_DIRECTORY/gitea-token")"
        export GITEA_MQ_WEBHOOK_SECRET="$(< "$CREDENTIALS_DIRECTORY/webhook-secret")"
        exec ${lib.getExe cfg.package}
      '';
    };
  };
}
