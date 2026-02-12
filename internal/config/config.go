package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Config holds all configuration for the gitea-mq service.
type Config struct {
	GiteaURL          string
	GiteaToken        string
	Repos             []RepoRef
	Topic             string // optional: discover repos by this Gitea topic
	DatabaseURL       string
	WebhookSecret     string
	ListenAddr        string
	WebhookPath       string
	ExternalURL       string // optional: external URL for webhook auto-setup
	PollInterval      time.Duration
	CheckTimeout      time.Duration
	RequiredChecks    []string
	RefreshInterval   time.Duration
	DiscoveryInterval time.Duration
	LogLevel          string // "debug", "info", "warn", "error"
}

// RepoRef identifies a repository by owner and name.
type RepoRef struct {
	Owner string
	Name  string
}

func (r RepoRef) String() string {
	return r.Owner + "/" + r.Name
}

// ParseRepoRef parses an "owner/name" string into a RepoRef.
// Returns false if the format is invalid.
func ParseRepoRef(s string) (RepoRef, bool) {
	owner, name, ok := strings.Cut(s, "/")
	if !ok || owner == "" || name == "" {
		return RepoRef{}, false
	}
	return RepoRef{Owner: owner, Name: name}, true
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
		repos, err := parseRepos(reposStr)
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

func parseRepos(s string) ([]RepoRef, error) {
	var repos []RepoRef
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		ref, ok := ParseRepoRef(part)
		if !ok {
			return nil, fmt.Errorf("invalid repo format %q, expected owner/name", part)
		}
		repos = append(repos, ref)
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
