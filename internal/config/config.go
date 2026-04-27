package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Mic92/gitea-mq/internal/forge"
)

// Config holds all configuration for the gitea-mq service.
type Config struct {
	GiteaURL          string
	GiteaToken        string
	Repos             []forge.RepoRef
	Topic             string // optional: discover repos by this Gitea topic
	DatabaseURL       string
	WebhookSecret     string
	ListenAddr        string
	WebhookPath       string
	ExternalURL       string // required: external URL for webhook auto-setup (GITEA_MQ_EXTERNAL_URL)
	PollInterval      time.Duration
	CheckTimeout      time.Duration
	RequiredChecks    []string
	RefreshInterval   time.Duration
	DiscoveryInterval time.Duration
	LogLevel          string // "debug", "info", "warn", "error"
}

// Load reads configuration from environment variables, validates required
// fields, and applies defaults.
func Load() (*Config, error) {
	cfg := &Config{
		ListenAddr:      envOrDefault("GITEA_MQ_LISTEN_ADDR", ":8080"),
		WebhookPath:     envOrDefault("GITEA_MQ_WEBHOOK_PATH", "/webhook"),
		PollInterval:    0,
		CheckTimeout:    0,
		RefreshInterval: 0,
	}

	// Required variables
	var missing []string

	cfg.GiteaURL = os.Getenv("GITEA_MQ_GITEA_URL")
	if cfg.GiteaURL == "" {
		missing = append(missing, "GITEA_MQ_GITEA_URL")
	}
	cfg.GiteaURL = strings.TrimRight(cfg.GiteaURL, "/")

	cfg.GiteaToken = os.Getenv("GITEA_MQ_GITEA_TOKEN")
	if cfg.GiteaToken == "" {
		missing = append(missing, "GITEA_MQ_GITEA_TOKEN")
	}

	cfg.Topic = os.Getenv("GITEA_MQ_TOPIC")

	reposStr := os.Getenv("GITEA_MQ_REPOS")
	if reposStr == "" && cfg.Topic == "" {
		missing = append(missing, "GITEA_MQ_REPOS")
	}

	cfg.DatabaseURL = os.Getenv("GITEA_MQ_DATABASE_URL")
	if cfg.DatabaseURL == "" {
		missing = append(missing, "GITEA_MQ_DATABASE_URL")
	}

	cfg.WebhookSecret = os.Getenv("GITEA_MQ_WEBHOOK_SECRET")
	if cfg.WebhookSecret == "" {
		missing = append(missing, "GITEA_MQ_WEBHOOK_SECRET")
	}

	cfg.ExternalURL = os.Getenv("GITEA_MQ_EXTERNAL_URL")
	if cfg.ExternalURL == "" {
		missing = append(missing, "GITEA_MQ_EXTERNAL_URL")
	}
	cfg.ExternalURL = strings.TrimRight(cfg.ExternalURL, "/")

	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	// Parse repos (may be empty when topic-based discovery is used).
	if reposStr != "" {
		repos, err := parseRepos(reposStr, forge.KindGitea)
		if err != nil {
			return nil, fmt.Errorf("GITEA_MQ_REPOS: %w", err)
		}
		cfg.Repos = repos
	}

	// Parse durations with defaults
	var err error
	cfg.PollInterval, err = parseDurationOrDefault("GITEA_MQ_POLL_INTERVAL", 30*time.Second)
	if err != nil {
		return nil, err
	}

	cfg.CheckTimeout, err = parseDurationOrDefault("GITEA_MQ_CHECK_TIMEOUT", 1*time.Hour)
	if err != nil {
		return nil, err
	}

	cfg.RefreshInterval, err = parseDurationOrDefault("GITEA_MQ_REFRESH_INTERVAL", 10*time.Second)
	if err != nil {
		return nil, err
	}

	cfg.DiscoveryInterval, err = parseDurationOrDefault("GITEA_MQ_DISCOVERY_INTERVAL", 5*time.Minute)
	if err != nil {
		return nil, err
	}

	// Optional: required checks fallback
	if checks := os.Getenv("GITEA_MQ_REQUIRED_CHECKS"); checks != "" {
		for _, c := range strings.Split(checks, ",") {
			c = strings.TrimSpace(c)
			if c != "" {
				cfg.RequiredChecks = append(cfg.RequiredChecks, c)
			}
		}
	}

	// Optional: log level
	cfg.LogLevel = envOrDefault("GITEA_MQ_LOG_LEVEL", "info")
	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
		// valid
	default:
		return nil, fmt.Errorf("GITEA_MQ_LOG_LEVEL: invalid value %q, must be one of: debug, info, warn, error", cfg.LogLevel)
	}

	return cfg, nil
}

func envOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// parseRepos parses a comma-separated list of "owner/name" entries and
// tags each with the given forge kind. Users never write the forge prefix;
// the env var name (GITEA_MQ_REPOS vs GITEA_MQ_GITHUB_REPOS) determines it.
func parseRepos(s string, kind forge.Kind) ([]forge.RepoRef, error) {
	var repos []forge.RepoRef
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		owner, name, ok := strings.Cut(part, "/")
		if !ok || owner == "" || name == "" {
			return nil, fmt.Errorf("invalid repo format %q, expected owner/name", part)
		}
		repos = append(repos, forge.RepoRef{Forge: kind, Owner: owner, Name: name})
	}
	if len(repos) == 0 {
		return nil, fmt.Errorf("no repos specified")
	}
	return repos, nil
}

func parseDurationOrDefault(envKey string, defaultVal time.Duration) (time.Duration, error) {
	s := os.Getenv(envKey)
	if s == "" {
		return defaultVal, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q: %w", envKey, s, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s: duration must be positive, got %v", envKey, d)
	}
	return d, nil
}
