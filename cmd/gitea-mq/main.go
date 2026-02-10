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

	"github.com/jogman/gitea-mq/internal/config"
	"github.com/jogman/gitea-mq/internal/gitea"
	"github.com/jogman/gitea-mq/internal/merge"
	"github.com/jogman/gitea-mq/internal/monitor"
	"github.com/jogman/gitea-mq/internal/poller"
	"github.com/jogman/gitea-mq/internal/queue"
	"github.com/jogman/gitea-mq/internal/setup"
	"github.com/jogman/gitea-mq/internal/store/pg"
	"github.com/jogman/gitea-mq/internal/web"
	"github.com/jogman/gitea-mq/internal/webhook"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slogLevel(),
	})))

	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func slogLevel() slog.Level {
	switch os.Getenv("GITEA_MQ_LOG_LEVEL") {
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

	slog.Info("starting gitea-mq",
		"listen", cfg.ListenAddr,
		"repos", cfg.Repos,
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
	giteaClient := gitea.NewHTTPClient(cfg.GiteaURL, cfg.GiteaToken)

	// Resolve webhook URL for auto-setup.
	webhookURL := cfg.GiteaURL + cfg.WebhookPath // fallback; in practice, the user configures the external URL
	if ext := os.Getenv("GITEA_MQ_EXTERNAL_URL"); ext != "" {
		webhookURL = ext + cfg.WebhookPath
	}

	// Per-repo setup: auto-setup, repo registration, cleanup.
	repoMonitors := make(map[string]*webhook.RepoMonitor, len(cfg.Repos))

	for _, ref := range cfg.Repos {
		// Auto-setup: ensure branch protection and webhook.
		if err := setup.EnsureRepo(ctx, giteaClient, ref.Owner, ref.Name, webhookURL, cfg.WebhookSecret); err != nil {
			slog.Warn("auto-setup failed", "repo", ref, "error", err)
			// Non-fatal: continue even if auto-setup fails.
		}

		// Ensure repo exists in DB.
		repo, err := queueSvc.GetOrCreateRepo(ctx, ref.Owner, ref.Name)
		if err != nil {
			return fmt.Errorf("register repo %s: %w", ref, err)
		}

		// Cleanup stale merge branches from previous runs.
		if err := merge.CleanupStaleBranches(ctx, giteaClient, queueSvc, ref.Owner, ref.Name, repo.ID); err != nil {
			slog.Warn("stale branch cleanup failed", "repo", ref, "error", err)
		}

		// Set up monitor deps for this repo.
		monDeps := &monitor.Deps{
			Gitea:          giteaClient,
			Queue:          queueSvc,
			Owner:          ref.Owner,
			Repo:           ref.Name,
			RepoID:         repo.ID,
			CheckTimeout:   cfg.CheckTimeout,
			FallbackChecks: cfg.RequiredChecks,
		}

		repoMonitors[ref.String()] = &webhook.RepoMonitor{
			Deps:   monDeps,
			RepoID: repo.ID,
		}

		// Start poller goroutine.
		pollerDeps := &poller.Deps{
			Gitea:          giteaClient,
			Queue:          queueSvc,
			RepoID:         repo.ID,
			Owner:          ref.Owner,
			Repo:           ref.Name,
			SuccessTimeout: 5 * time.Minute,
		}
		go poller.Run(ctx, pollerDeps, cfg.PollInterval)
	}

	// HTTP server: webhook + dashboard on the same mux.
	mux := http.NewServeMux()

	// Webhook handler.
	webhookHandler := webhook.Handler(cfg.WebhookSecret, repoMonitors, queueSvc)
	mux.Handle(cfg.WebhookPath, webhookHandler)

	// Health check.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	// Dashboard.
	webDeps := &web.Deps{
		Queue:           queueSvc,
		ManagedRepos:    cfg.Repos,
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
