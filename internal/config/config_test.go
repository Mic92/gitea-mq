package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mic92/gitea-mq/internal/forge"
)

func TestParseRepos_TagsForgeKind(t *testing.T) {
	got, err := parseRepos("org/app, org/lib", forge.KindGitea)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	for _, r := range got {
		if r.Forge != forge.KindGitea {
			t.Errorf("%s: Forge = %q, want gitea", r, r.Forge)
		}
	}
	if got[0].String() != "gitea:org/app" {
		t.Errorf("String() = %q, want gitea:org/app", got[0].String())
	}
}

func TestParseRepos_Invalid(t *testing.T) {
	if _, err := parseRepos("noslash", forge.KindGitea); err == nil {
		t.Fatal("expected error for missing slash")
	}
}

// setEnv resets every GITEA_MQ_* variable and applies the given map so each
// subtest starts from a known-empty environment.
func setEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GITEA_MQ_") {
			t.Setenv(k, "")
		}
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
}

var baseEnv = map[string]string{
	"GITEA_MQ_DATABASE_URL": "postgres://x",
	"GITEA_MQ_EXTERNAL_URL": "https://mq.example.com",
}

func with(extra map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range baseEnv {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func TestLoad_NoForgeFails(t *testing.T) {
	setEnv(t, baseEnv)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "no forge configured") {
		t.Fatalf("expected no-forge error, got %v", err)
	}
}

func TestLoad_GithubOnlyOK(t *testing.T) {
	setEnv(t, with(map[string]string{
		"GITEA_MQ_GITHUB_APP_ID":         "12345",
		"GITEA_MQ_GITHUB_PRIVATE_KEY":    "-----BEGIN PRIVATE KEY-----\nfake\n-----END PRIVATE KEY-----",
		"GITEA_MQ_GITHUB_WEBHOOK_SECRET": "ghsecret",
		"GITEA_MQ_GITHUB_REPOS":          "org/app",
	}))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Gitea != nil {
		t.Error("expected Gitea nil when GITEA_URL unset")
	}
	if cfg.Github == nil || cfg.Github.AppID != 12345 {
		t.Fatalf("Github = %+v", cfg.Github)
	}
	if len(cfg.Github.Repos) != 1 || cfg.Github.Repos[0].Forge != forge.KindGithub {
		t.Errorf("Github.Repos = %+v, want one github ref", cfg.Github.Repos)
	}
	// PollInterval inherits the global default.
	if cfg.Github.PollInterval != 30*time.Second {
		t.Errorf("Github.PollInterval = %v, want 30s (inherited)", cfg.Github.PollInterval)
	}
}

func TestLoad_GithubAppIDWithoutKeyFails(t *testing.T) {
	setEnv(t, with(map[string]string{
		"GITEA_MQ_GITHUB_APP_ID":         "12345",
		"GITEA_MQ_GITHUB_WEBHOOK_SECRET": "s",
	}))
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "GITEA_MQ_GITHUB_PRIVATE_KEY") {
		t.Fatalf("expected missing private key error, got %v", err)
	}
}

func TestLoad_GithubPrivateKeyFile(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(keyPath, []byte("PEMDATA"), 0o600); err != nil {
		t.Fatal(err)
	}
	setEnv(t, with(map[string]string{
		"GITEA_MQ_GITHUB_APP_ID":           "1",
		"GITEA_MQ_GITHUB_PRIVATE_KEY_FILE": keyPath,
		"GITEA_MQ_GITHUB_WEBHOOK_SECRET":   "s",
	}))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(cfg.Github.PrivateKey) != "PEMDATA" {
		t.Errorf("PrivateKey = %q, want PEMDATA", cfg.Github.PrivateKey)
	}
}

func TestLoad_GithubPollIntervalOverride(t *testing.T) {
	setEnv(t, with(map[string]string{
		"GITEA_MQ_GITHUB_APP_ID":         "1",
		"GITEA_MQ_GITHUB_PRIVATE_KEY":    "k",
		"GITEA_MQ_GITHUB_WEBHOOK_SECRET": "s",
		"GITEA_MQ_POLL_INTERVAL":         "1m",
		"GITEA_MQ_GITHUB_POLL_INTERVAL":  "5s",
	}))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Github.PollInterval != 5*time.Second {
		t.Errorf("Github.PollInterval = %v, want 5s", cfg.Github.PollInterval)
	}
	if cfg.PollInterval != time.Minute {
		t.Errorf("PollInterval = %v, want 1m", cfg.PollInterval)
	}
}

func TestLoad_GiteaOnlyStillWorks(t *testing.T) {
	setEnv(t, with(map[string]string{
		"GITEA_MQ_GITEA_URL":      "https://gitea.example.com/",
		"GITEA_MQ_GITEA_TOKEN":    "tok",
		"GITEA_MQ_WEBHOOK_SECRET": "sec",
		"GITEA_MQ_REPOS":          "org/app",
	}))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Github != nil {
		t.Error("expected Github nil")
	}
	if cfg.Gitea == nil || cfg.Gitea.URL != "https://gitea.example.com" {
		t.Fatalf("Gitea = %+v", cfg.Gitea)
	}
	if len(cfg.Repos()) != 1 || cfg.Repos()[0].Forge != forge.KindGitea {
		t.Errorf("Repos() = %+v", cfg.Repos())
	}
}
