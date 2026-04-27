package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Mic92/gitea-mq/internal/config"
	"github.com/Mic92/gitea-mq/internal/discovery"
	"github.com/Mic92/gitea-mq/internal/forge"
	"github.com/Mic92/gitea-mq/internal/gitea"
	"github.com/Mic92/gitea-mq/internal/github"
	"github.com/Mic92/gitea-mq/internal/queue"
	"github.com/Mic92/gitea-mq/internal/registry"
	"github.com/Mic92/gitea-mq/internal/store/pg"
	"github.com/Mic92/gitea-mq/internal/web"
	"github.com/Mic92/gitea-mq/internal/webhook"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func slogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slogLevel(cfg.LogLevel),
	})))

	slog.Info("starting gitea-mq",
		"listen", cfg.ListenAddr,
		"repos", cfg.Repos(),
		"gitea", cfg.Gitea != nil,
		"github", cfg.Github != nil,
		"poll_interval", cfg.PollInterval,
		"check_timeout", cfg.CheckTimeout,
	)

	// Graceful shutdown context.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Database.
	pool, err := pg.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	queueSvc := queue.NewService(pool)
	forges := forge.NewSet()
	var discSources []discovery.Source

	var giteaWebhookSecret string
	if cfg.Gitea != nil {
		giteaClient := gitea.NewHTTPClient(cfg.Gitea.URL, cfg.Gitea.Token)
		forges.Register(gitea.NewForge(giteaClient, cfg.Gitea.URL))
		giteaWebhookSecret = cfg.Gitea.WebhookSecret
		if cfg.Gitea.Topic != "" {
			discSources = append(discSources, discovery.Source{
				Kind: forge.KindGitea,
				List: gitea.TopicSource(giteaClient, cfg.Gitea.Topic),
			})
		}
	}

	if cfg.Github != nil {
		app, err := github.NewApp(cfg.Github.AppID, cfg.Github.PrivateKey, github.DefaultBaseURL)
		if err != nil {
			return fmt.Errorf("init github app: %w", err)
		}
		// Populate the installation→repo map before any forge call so the
		// initial registry.Add for explicit repos can resolve a client.
		if err := app.Refresh(ctx); err != nil {
			slog.Warn("github: initial installation refresh failed", "err", err)
		}
		if err := app.SyncHookConfig(ctx, cfg.ExternalURL, cfg.Github.WebhookSecret); err != nil {
			slog.Warn("github: sync app webhook config failed", "err", err)
		}
		forges.Register(github.NewForge(app, ""))
		discSources = append(discSources, discovery.Source{
			Kind: forge.KindGithub,
			List: github.InstallationSource(app),
		})
	}

	// Create the repo registry — central coordination for managed repos.
	reg := registry.New(ctx, &registry.Deps{
		Forges:         forges,
		Queue:          queueSvc,
		WebhookSecret:  giteaWebhookSecret,
		ExternalURL:    cfg.ExternalURL,
		PollInterval:   cfg.PollInterval,
		CheckTimeout:   cfg.CheckTimeout,
		FallbackChecks: cfg.RequiredChecks,
		SuccessTimeout: 5 * time.Minute,
	})

	discTrigger := make(chan struct{}, 1)
	discDeps := &discovery.Deps{
		Sources:       discSources,
		Registry:      reg,
		ExplicitRepos: cfg.Repos(),
		Trigger:       discTrigger,
	}
	// Initial discovery blocks startup so the dashboard and webhook see the
	// full repo set immediately. Explicit repos are added via the same path.
	discovery.DiscoverOnce(ctx, discDeps)
	if len(discSources) > 0 {
		go discovery.Run(ctx, discDeps, cfg.DiscoveryInterval)
	}

	// HTTP server: webhook + dashboard on the same mux.
	mux := http.NewServeMux()

	if cfg.Gitea != nil {
		h := webhook.Handler(giteaWebhookSecret, reg, queueSvc)
		mux.Handle("/webhook/gitea", h)
		// Legacy alias kept so existing per-repo webhooks created by earlier
		// versions keep working.
		if cfg.WebhookPath != "" && cfg.WebhookPath != "/webhook/gitea" {
			mux.Handle(cfg.WebhookPath, h)
		}
	}
	if cfg.Github != nil {
		mux.Handle("/webhook/github", webhook.GithubHandler(
			[]byte(cfg.Github.WebhookSecret), reg, queueSvc,
			func() {
				select {
				case discTrigger <- struct{}{}:
				default:
				}
			}))
	}

	// Health check.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	// Dashboard — uses registry for dynamic repo listing.
	webDeps := &web.Deps{
		Queue:           queueSvc,
		Repos:           reg,
		Forges:          forges,
		FallbackChecks:  cfg.RequiredChecks,
		RefreshInterval: int(cfg.RefreshInterval.Seconds()),
	}
	dashMux := web.NewMux(webDeps)
	// Mount dashboard routes — the web mux handles /, /repo/, /static/.
	mux.Handle("/static/", dashMux)
	mux.Handle("/repo/", dashMux)
	// Root must be last to avoid overriding other routes.
	mux.Handle("/", dashMux)

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Start HTTP server.
	errCh := make(chan error, 1)
	go func() {
		slog.Info("HTTP server listening", "addr", cfg.ListenAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	// Wait for shutdown signal or server error.
	select {
	case <-ctx.Done():
		slog.Info("shutting down")
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("HTTP server: %w", err)
		}
	}

	// Graceful shutdown with timeout.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("HTTP server shutdown: %w", err)
	}

	slog.Info("shutdown complete")

	return nil
}
