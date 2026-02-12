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
	"github.com/jogman/gitea-mq/internal/discovery"
	"github.com/jogman/gitea-mq/internal/gitea"
	"github.com/jogman/gitea-mq/internal/queue"
	"github.com/jogman/gitea-mq/internal/registry"
	"github.com/jogman/gitea-mq/internal/store/pg"
	"github.com/jogman/gitea-mq/internal/web"
	"github.com/jogman/gitea-mq/internal/webhook"
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
		"repos", cfg.Repos,
		"topic", cfg.Topic,
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
	var webhookURL string
	if cfg.ExternalURL != "" {
		webhookURL = cfg.ExternalURL + cfg.WebhookPath
	}

	// Create the repo registry — central coordination for managed repos.
	reg := registry.New(ctx, &registry.Deps{
		Gitea:          giteaClient,
		Queue:          queueSvc,
		WebhookURL:     webhookURL,
		WebhookSecret:  cfg.WebhookSecret,
		ExternalURL:    cfg.ExternalURL,
		PollInterval:   cfg.PollInterval,
		CheckTimeout:   cfg.CheckTimeout,
		FallbackChecks: cfg.RequiredChecks,
		SuccessTimeout: 5 * time.Minute,
	})

	// Register explicit repos.
	for _, ref := range cfg.Repos {
		if err := reg.Add(ctx, ref); err != nil {
			return fmt.Errorf("register repo %s: %w", ref, err)
		}
	}

	// Topic-based discovery: run initial discovery, then start background loop.
	if cfg.Topic != "" {
		discDeps := &discovery.Deps{
			Gitea:         giteaClient,
			Registry:      reg,
			Topic:         cfg.Topic,
			ExplicitRepos: cfg.Repos,
		}

		// Initial discovery blocks startup so all repos are ready before serving.
		if err := discovery.DiscoverOnce(ctx, discDeps); err != nil {
			slog.Warn("initial discovery failed, continuing with explicit repos", "error", err)
		}

		// Background discovery loop.
		go discovery.Run(ctx, discDeps, cfg.DiscoveryInterval)
	}

	// HTTP server: webhook + dashboard on the same mux.
	mux := http.NewServeMux()

	// Webhook handler — uses registry for dynamic repo lookup.
	webhookHandler := webhook.Handler(cfg.WebhookSecret, reg, queueSvc)
	mux.Handle(cfg.WebhookPath, webhookHandler)

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
		Gitea:           giteaClient,
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
