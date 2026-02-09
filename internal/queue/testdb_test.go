package queue_test

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
	"github.com/jogman/gitea-mq/internal/store/pg"
)

var (
	testPostgresServer *postgresServer //nolint:gochecknoglobals
	testDBCount        atomic.Int32    //nolint:gochecknoglobals
)

type postgresServer struct {
	cmd     *exec.Cmd
	tempDir string
}

func (s *postgresServer) Cleanup() {
	defer func() {
		if err := os.RemoveAll(s.tempDir); err != nil {
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
		if err := syscall.Kill(pgid, syscall.SIGKILL); err != nil {
			slog.Error("failed to kill process", "error", err)
		}
	})

	if err := syscall.Kill(pgid, syscall.SIGTERM); err != nil {
		slog.Error("failed to terminate process", "error", err)
	}

	if err := cmd.Wait(); err != nil {
		slog.Error("failed to wait for process", "error", err)
	}
}

func startPostgresServer(ctx context.Context) (*postgresServer, error) {
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

	server := &postgresServer{cmd: postgresProc, tempDir: tempDir}

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

func TestMain(m *testing.M) {
	os.Exit(innerTestMain(m))
}

func innerTestMain(m *testing.M) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_ = os.Unsetenv("PGDATABASE")
	_ = os.Unsetenv("PGUSER")
	_ = os.Unsetenv("PGHOST")

	var err error

	testPostgresServer, err = startPostgresServer(ctx)
	if err != nil {
		slog.Error("failed to start postgres", "error", err)

		return 1
	}

	defer testPostgresServer.Cleanup()

	return m.Run()
}

// newTestDB creates a fresh database and returns a connected pool.
func newTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	if testPostgresServer == nil {
		t.Fatal("postgres server not started")
	}

	dbName := fmt.Sprintf("testdb%d", testDBCount.Add(1))

	cmd := exec.CommandContext(t.Context(), "createdb",
		"-h", testPostgresServer.tempDir,
		"-U", "postgres",
		dbName,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("createdb: %v", err)
	}

	connStr := fmt.Sprintf("postgres://?dbname=%s&user=postgres&host=%s", dbName, testPostgresServer.tempDir)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	pool, err := pg.Connect(ctx, connStr)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	t.Cleanup(pool.Close)

	return pool
}
