package config

import (
	"testing"
)

// setEnv sets the minimum required env vars for config loading, then applies overrides.
func setEnv(t *testing.T, overrides map[string]string) {
	t.Helper()

	defaults := map[string]string{
		"GITEA_MQ_GITEA_URL":          "https://gitea.example.com",
		"GITEA_MQ_GITEA_TOKEN":        "test-token",
		"GITEA_MQ_DATABASE_URL":       "postgres:///test",
		"GITEA_MQ_WEBHOOK_SECRET":     "secret",
		"GITEA_MQ_REPOS":              "",
		"GITEA_MQ_TOPIC":              "",
		"GITEA_MQ_DISCOVERY_INTERVAL": "",
	}

	for k, v := range overrides {
		defaults[k] = v
	}

	for k, v := range defaults {
		t.Setenv(k, v)
	}
}

func TestLoadReposOnly(t *testing.T) {
	setEnv(t, map[string]string{
		"GITEA_MQ_REPOS": "org/app,org/lib",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Repos) != 2 {
		t.Fatalf("expected 2 repos, got %d", len(cfg.Repos))
	}
	if cfg.Repos[0].Owner != "org" || cfg.Repos[0].Name != "app" {
		t.Errorf("unexpected first repo: %v", cfg.Repos[0])
	}
}

func TestLoadTopicOnly(t *testing.T) {
	setEnv(t, map[string]string{
		"GITEA_MQ_TOPIC": "merge-queue",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Topic != "merge-queue" {
		t.Errorf("expected topic 'merge-queue', got %q", cfg.Topic)
	}
	if len(cfg.Repos) != 0 {
		t.Errorf("expected 0 repos in topic-only mode, got %d", len(cfg.Repos))
	}
}

func TestLoadTopicAndRepos(t *testing.T) {
	setEnv(t, map[string]string{
		"GITEA_MQ_TOPIC": "merge-queue",
		"GITEA_MQ_REPOS": "org/legacy",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Topic != "merge-queue" {
		t.Errorf("expected topic 'merge-queue', got %q", cfg.Topic)
	}
	if len(cfg.Repos) != 1 || cfg.Repos[0].String() != "org/legacy" {
		t.Errorf("expected [org/legacy], got %v", cfg.Repos)
	}
}

func TestLoadNeitherTopicNorRepos(t *testing.T) {
	setEnv(t, map[string]string{})

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when neither topic nor repos is set")
	}
}

func TestLoadDiscoveryInterval(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		setEnv(t, map[string]string{
			"GITEA_MQ_TOPIC": "merge-queue",
		})

		cfg, err := Load()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.DiscoveryInterval.Minutes() != 5 {
			t.Errorf("expected 5m default, got %v", cfg.DiscoveryInterval)
		}
	})

	t.Run("custom", func(t *testing.T) {
		setEnv(t, map[string]string{
			"GITEA_MQ_TOPIC":              "merge-queue",
			"GITEA_MQ_DISCOVERY_INTERVAL": "2m",
		})

		cfg, err := Load()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.DiscoveryInterval.Minutes() != 2 {
			t.Errorf("expected 2m, got %v", cfg.DiscoveryInterval)
		}
	})
}
