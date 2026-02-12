// Package testutil provides shared test infrastructure for packages that
// need a real PostgreSQL instance. Each test package calls
// StartPostgresServer from its own TestMain and uses NewTestDB to get an
// isolated database per test.
package testutil

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jogman/gitea-mq/internal/queue"
	"github.com/jogman/gitea-mq/internal/store/pg"
)

// PostgresServer wraps a temporary postgres instance.
type PostgresServer struct {
	cmd     *exec.Cmd
	TempDir string
	dbCount atomic.Int32
}

// Cleanup terminates the postgres process and removes the temp directory.
func (s *PostgresServer) Cleanup() {
	defer func() {
		if err := os.RemoveAll(s.TempDir); err != nil {
			slog.Warn("failed to remove postgres temp directory", "error", err)
		}
	}()

	terminateProcess(s.cmd)
}

func terminateProcess(cmd *exec.Cmd) {
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		slog.Error("failed to get pgid", "error", err)

		return
	}

	time.AfterFunc(10*time.Second, func() {
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
			slog.Error("failed to kill process", "error", err)
		}
	})

	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		slog.Error("failed to terminate process", "error", err)
	}

	if err := cmd.Wait(); err != nil {
		slog.Error("failed to wait for process", "error", err)
	}
}

// StartPostgresServer launches a temporary postgres instance using Unix
// sockets in a temp directory. Call Cleanup when done.
func StartPostgresServer(ctx context.Context) (*PostgresServer, error) {
	tempDir, err := os.MkdirTemp("", "gitea-mq-test-postgres")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}

	defer func() {
		if err != nil {
			_ = os.RemoveAll(tempDir)
		}
	}()

	dbPath := filepath.Join(tempDir, "data")
	initdb := exec.CommandContext(ctx, "initdb", "-D", dbPath, "-U", "postgres")
	initdb.Stdout = os.Stdout
	initdb.Stderr = os.Stderr

	if err = initdb.Run(); err != nil {
		return nil, fmt.Errorf("initdb: %w", err)
	}

	postgresProc := exec.CommandContext(ctx, "postgres",
		"-D", dbPath,
		"-k", tempDir,
		"-c", "listen_addresses=",
	)
	postgresProc.Stdout = os.Stdout
	postgresProc.Stderr = os.Stderr
	postgresProc.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}

	if err = postgresProc.Start(); err != nil {
		return nil, fmt.Errorf("start postgres: %w", err)
	}

	server := &PostgresServer{cmd: postgresProc, TempDir: tempDir}

	defer func() {
		if err != nil {
			server.Cleanup()
		}
	}()

	for range 30 {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("timeout waiting for postgres: %w", ctx.Err())
		}

		check := exec.CommandContext(ctx, "pg_isready", "-h", tempDir, "-U", "postgres")

		if err = check.Run(); err == nil {
			break
		}

		time.Sleep(100 * time.Millisecond)
	}

	if err != nil {
		return nil, fmt.Errorf("postgres not ready: %w", err)
	}

	return server, nil
}

// NewTestDB creates a fresh database on the given server and returns a
// connected, migrated pool. The pool is closed when the test finishes.
func NewTestDB(t *testing.T, server *PostgresServer) *pgxpool.Pool {
	t.Helper()

	if server == nil {
		t.Fatal("postgres server not started")
	}

	dbName := fmt.Sprintf("testdb%d", server.dbCount.Add(1))

	cmd := exec.CommandContext(t.Context(), "createdb",
		"-h", server.TempDir,
		"-U", "postgres",
		dbName,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("createdb: %v", err)
	}

	connStr := fmt.Sprintf("postgres://?dbname=%s&user=postgres&host=%s", dbName, server.TempDir)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	pool, err := pg.Connect(ctx, connStr)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	t.Cleanup(pool.Close)

	return pool
}

// RunWithPostgres is a helper for TestMain: starts postgres, runs tests,
// cleans up. Returns the exit code for os.Exit.
func RunWithPostgres(m *testing.M) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_ = os.Unsetenv("PGDATABASE")
	_ = os.Unsetenv("PGUSER")
	_ = os.Unsetenv("PGHOST")

	server, err := StartPostgresServer(ctx)
	if err != nil {
		slog.Error("failed to start postgres", "error", err)

		return 1
	}

	defer server.Cleanup()

	// Store in package-level var so NewTestDB can find it.
	// Each test package has its own copy of this via SetServer.
	globalServer = server

	return m.Run()
}

//nolint:gochecknoglobals
var globalServer *PostgresServer

// Server returns the shared postgres server started by RunWithPostgres.
func Server() *PostgresServer {
	return globalServer
}

// TestDB is a convenience wrapper that creates a fresh test database on the
// shared server. Equivalent to NewTestDB(t, Server()).
func TestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	return NewTestDB(t, Server())
}

// TestQueueService creates a fresh test database, queue service, and repo
// row (owner="org", name="app"). Returns the service, a context, and the
// repo ID. This is the common preamble shared by most test packages.
func TestQueueService(t *testing.T) (*queue.Service, context.Context, int64) {
	t.Helper()

	pool := TestDB(t)
	svc := queue.NewService(pool)
	ctx := t.Context()

	repo, err := svc.GetOrCreateRepo(ctx, "org", "app")
	if err != nil {
		t.Fatalf("create test repo: %v", err)
	}

	return svc, ctx, repo.ID
}
