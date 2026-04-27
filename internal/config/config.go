package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Mic92/gitea-mq/internal/forge"
)

// Config holds all configuration for the gitea-mq service.
type Config struct {
	Gitea  *GiteaConfig  // nil if unconfigured
	Github *GithubConfig // nil if unconfigured

	DatabaseURL       string
	ListenAddr        string
	WebhookPath       string
	ExternalURL       string
	PollInterval      time.Duration
	CheckTimeout      time.Duration
	RequiredChecks    []string
	RefreshInterval   time.Duration
	DiscoveryInterval time.Duration
	LogLevel          string
}

type GiteaConfig struct {
	URL           string
	Token         string
	WebhookSecret string
	Topic         string
	Repos         []forge.RepoRef
}

type GithubConfig struct {
	AppID         int64
	PrivateKey    []byte
	WebhookSecret string
	Repos         []forge.RepoRef
	// PollInterval defaults to Config.PollInterval; override via
	// GITEA_MQ_GITHUB_POLL_INTERVAL when GitHub's higher rate limit
	// warrants a different cadence.
	PollInterval time.Duration
}

func (c *Config) Repos() []forge.RepoRef {
	var out []forge.RepoRef
	if c.Gitea != nil {
		out = append(out, c.Gitea.Repos...)
	}
	if c.Github != nil {
		out = append(out, c.Github.Repos...)
	}
	return out
}

// Load reads configuration from environment variables, validates required
// fields, and applies defaults.
func Load() (*Config, error) {
	cfg := &Config{
		ListenAddr:  envOrDefault("GITEA_MQ_LISTEN_ADDR", ":8080"),
		WebhookPath: envOrDefault("GITEA_MQ_WEBHOOK_PATH", "/webhook"),
	}

	var missing []string

	cfg.DatabaseURL = os.Getenv("GITEA_MQ_DATABASE_URL")
	if cfg.DatabaseURL == "" {
		missing = append(missing, "GITEA_MQ_DATABASE_URL")
	}

	cfg.ExternalURL = strings.TrimRight(os.Getenv("GITEA_MQ_EXTERNAL_URL"), "/")
	if cfg.ExternalURL == "" {
		missing = append(missing, "GITEA_MQ_EXTERNAL_URL")
	}

	var err error
	cfg.Gitea, err = loadGitea(&missing)
	if err != nil {
		return nil, err
	}
	cfg.Github, err = loadGithub(&missing)
	if err != nil {
		return nil, err
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	if cfg.Gitea == nil && cfg.Github == nil {
		return nil, fmt.Errorf("no forge configured: set GITEA_MQ_GITEA_URL or GITEA_MQ_GITHUB_APP_ID")
	}

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

	if cfg.Github != nil {
		cfg.Github.PollInterval, err = parseDurationOrDefault("GITEA_MQ_GITHUB_POLL_INTERVAL", cfg.PollInterval)
		if err != nil {
			return nil, err
		}
	}

	if checks := os.Getenv("GITEA_MQ_REQUIRED_CHECKS"); checks != "" {
		for _, c := range strings.Split(checks, ",") {
			if c = strings.TrimSpace(c); c != "" {
				cfg.RequiredChecks = append(cfg.RequiredChecks, c)
			}
		}
	}

	cfg.LogLevel = envOrDefault("GITEA_MQ_LOG_LEVEL", "info")
	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return nil, fmt.Errorf("GITEA_MQ_LOG_LEVEL: invalid value %q, must be one of: debug, info, warn, error", cfg.LogLevel)
	}

	return cfg, nil
}

// loadGitea returns a GiteaConfig if GITEA_MQ_GITEA_URL is set; otherwise nil.
// Dependent variables are reported missing only when Gitea is configured so a
// GitHub-only deployment carries no Gitea baggage.
func loadGitea(missing *[]string) (*GiteaConfig, error) {
	url := strings.TrimRight(os.Getenv("GITEA_MQ_GITEA_URL"), "/")
	if url == "" {
		return nil, nil
	}
	gc := &GiteaConfig{
		URL:   url,
		Topic: os.Getenv("GITEA_MQ_TOPIC"),
	}

	gc.Token = os.Getenv("GITEA_MQ_GITEA_TOKEN")
	if gc.Token == "" {
		*missing = append(*missing, "GITEA_MQ_GITEA_TOKEN")
	}
	gc.WebhookSecret = os.Getenv("GITEA_MQ_WEBHOOK_SECRET")
	if gc.WebhookSecret == "" {
		*missing = append(*missing, "GITEA_MQ_WEBHOOK_SECRET")
	}

	reposStr := os.Getenv("GITEA_MQ_REPOS")
	if reposStr == "" && gc.Topic == "" {
		*missing = append(*missing, "GITEA_MQ_REPOS")
	}
	if reposStr != "" {
		repos, err := parseRepos(reposStr, forge.KindGitea)
		if err != nil {
			return nil, fmt.Errorf("GITEA_MQ_REPOS: %w", err)
		}
		gc.Repos = repos
	}
	return gc, nil
}

func loadGithub(missing *[]string) (*GithubConfig, error) {
	appIDStr := os.Getenv("GITEA_MQ_GITHUB_APP_ID")
	if appIDStr == "" {
		return nil, nil
	}
	appID, err := strconv.ParseInt(appIDStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("GITEA_MQ_GITHUB_APP_ID: %w", err)
	}
	gc := &GithubConfig{AppID: appID}

	gc.PrivateKey, err = readSecret("GITEA_MQ_GITHUB_PRIVATE_KEY")
	if err != nil {
		return nil, err
	}
	if len(gc.PrivateKey) == 0 {
		*missing = append(*missing, "GITEA_MQ_GITHUB_PRIVATE_KEY")
	}

	gc.WebhookSecret = os.Getenv("GITEA_MQ_GITHUB_WEBHOOK_SECRET")
	if gc.WebhookSecret == "" {
		*missing = append(*missing, "GITEA_MQ_GITHUB_WEBHOOK_SECRET")
	}

	if reposStr := os.Getenv("GITEA_MQ_GITHUB_REPOS"); reposStr != "" {
		gc.Repos, err = parseRepos(reposStr, forge.KindGithub)
		if err != nil {
			return nil, fmt.Errorf("GITEA_MQ_GITHUB_REPOS: %w", err)
		}
	}
	return gc, nil
}

// readSecret reads <key> or, if unset, the file at <key>_FILE. The _FILE form
// keeps multi-line PEM keys out of process environment listings.
func readSecret(key string) ([]byte, error) {
	if v := os.Getenv(key); v != "" {
		return []byte(v), nil
	}
	if path := os.Getenv(key + "_FILE"); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s_FILE: %w", key, err)
		}
		return b, nil
	}
	return nil, nil
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
