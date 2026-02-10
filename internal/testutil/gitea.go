// Package testutil provides shared test infrastructure.
// This file provides a helper to start a real Gitea instance for integration
// tests. It uses SQLite so only the gitea binary and git are needed.
package testutil

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// GiteaServer wraps a temporary Gitea instance.
type GiteaServer struct {
	cmd     *exec.Cmd
	TempDir string
	URL     string // e.g. "http://127.0.0.1:3000"
	Port    int
}

// PatchRepoHooks rewrites git hook shebangs in a Gitea repo from
// #!/usr/bin/env bash to #!<absolute-path-to-bash>. Gitea generates hooks
// with #!/usr/bin/env bash, which fails in nix build sandboxes where
// /usr/bin/env doesn't exist. We always patch so tests work everywhere.
func (s *GiteaServer) PatchRepoHooks(owner, repo string) error {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		return fmt.Errorf("bash not in PATH: %w", err)
	}

	repoDir := filepath.Join(s.TempDir, "data", "gitea-repositories", owner, repo+".git", "hooks")

	return filepath.Walk(repoDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if info.IsDir() {
			return nil
		}

		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}

		oldShebang := "#!/usr/bin/env bash"
		newShebang := "#!" + bashPath

		s := string(content)
		if strings.HasPrefix(s, oldShebang) {
			patched := newShebang + s[len(oldShebang):]
			fmt.Fprintf(os.Stderr, "DEBUG: patching hook shebang in %s: %s -> %s\n", path, oldShebang, newShebang)

			return os.WriteFile(path, []byte(patched), info.Mode())
		}

		return nil
	})
}

// Cleanup terminates the Gitea process and removes the temp directory.
func (s *GiteaServer) Cleanup() {
	defer func() {
		if err := os.RemoveAll(s.TempDir); err != nil {
			fmt.Fprintf(os.Stderr, "failed to remove gitea temp directory: %v\n", err)
		}
	}()

	terminateProcess(s.cmd)
}

// StartGiteaServer launches a temporary Gitea instance on a random port
// using SQLite as the database. Call Cleanup when done.
func StartGiteaServer(ctx context.Context) (*GiteaServer, error) {
	tempDir, err := os.MkdirTemp("", "gitea-mq-test-gitea")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}

	defer func() {
		if err != nil {
			_ = os.RemoveAll(tempDir)
		}
	}()

	// Find a free port.
	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("find free port: %w", err)
	}

	// Write a minimal app.ini.
	customDir := filepath.Join(tempDir, "custom", "conf")
	if err := os.MkdirAll(customDir, 0o755); err != nil {
		return nil, fmt.Errorf("create custom dir: %w", err)
	}

	appIni := fmt.Sprintf(`
[database]
DB_TYPE = sqlite3
PATH = %s/gitea.db

[server]
HTTP_PORT = %d
ROOT_URL = http://127.0.0.1:%d/
PROTOCOL = http
LFS_START_SERVER = false

[service]
DISABLE_REGISTRATION = false
REQUIRE_SIGNIN_VIEW = false
NO_REPLY_ADDRESS = localhost

[security]
INSTALL_LOCK = true
SECRET_KEY = test-secret-key-for-testing-only

[log]
MODE = console
LEVEL = Warn

[webhook]
ALLOWED_HOST_LIST = loopback
`, tempDir, port, port)

	if err := os.WriteFile(filepath.Join(customDir, "app.ini"), []byte(appIni), 0o644); err != nil {
		return nil, fmt.Errorf("write app.ini: %w", err)
	}

	// Set up git config for Gitea's internal git operations.
	// Gitea uses <APP_DATA_PATH>/home as HOME for git (setting.Git.HomePath).
	giteaGitHome := filepath.Join(tempDir, "data", "home")
	if err := os.MkdirAll(giteaGitHome, 0o755); err != nil {
		return nil, fmt.Errorf("create gitea git home dir: %w", err)
	}

	gitConfig := "[user]\n\tname = Gitea Test\n\temail = test@test.com\n"
	if err := os.WriteFile(filepath.Join(giteaGitHome, ".gitconfig"), []byte(gitConfig), 0o644); err != nil {
		return nil, fmt.Errorf("write .gitconfig: %w", err)
	}

	// Also set up a git home for the test process itself (for MergeBranches etc.).
	gitConfigDir := filepath.Join(tempDir, "git-home")
	if err := os.MkdirAll(gitConfigDir, 0o755); err != nil {
		return nil, fmt.Errorf("create git-home dir: %w", err)
	}

	if err := os.WriteFile(filepath.Join(gitConfigDir, ".gitconfig"), []byte(gitConfig), 0o644); err != nil {
		return nil, fmt.Errorf("write test .gitconfig: %w", err)
	}

	// Start Gitea.
	giteaProc := exec.CommandContext(ctx, "gitea", "web")
	giteaProc.Env = append(os.Environ(),
		"GITEA_WORK_DIR="+tempDir,
		"GITEA_CUSTOM="+filepath.Join(tempDir, "custom"),
		"HOME="+gitConfigDir,
	)
	giteaProc.Stdout = os.Stdout
	giteaProc.Stderr = os.Stderr
	giteaProc.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}

	if err = giteaProc.Start(); err != nil {
		return nil, fmt.Errorf("start gitea: %w", err)
	}

	server := &GiteaServer{
		cmd:     giteaProc,
		TempDir: tempDir,
		URL:     fmt.Sprintf("http://127.0.0.1:%d", port),
		Port:    port,
	}

	defer func() {
		if err != nil {
			server.Cleanup()
		}
	}()

	// Wait for Gitea to be ready.
	httpClient := &http.Client{Timeout: 2 * time.Second}

	for range 60 {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("timeout waiting for gitea: %w", ctx.Err())
		}

		resp, httpErr := httpClient.Get(server.URL + "/api/v1/version")
		if httpErr == nil {
			if err := resp.Body.Close(); err != nil {
				slog.Warn("failed to close response body", "error", err)
			}

			if resp.StatusCode == http.StatusOK {
				break
			}
		}

		time.Sleep(500 * time.Millisecond)
	}

	// Create admin user.
	createAdmin := exec.CommandContext(ctx, "gitea", "admin", "user", "create",
		"--admin",
		"--username", "testuser",
		"--password", "testpass123",
		"--email", "test@test.com",
	)
	createAdmin.Env = append(os.Environ(),
		"GITEA_WORK_DIR="+tempDir,
		"GITEA_CUSTOM="+filepath.Join(tempDir, "custom"),
	)
	createAdmin.Stdout = os.Stdout
	createAdmin.Stderr = os.Stderr

	if err = createAdmin.Run(); err != nil {
		return nil, fmt.Errorf("create admin user: %w", err)
	}

	return server, nil
}

// freePort finds a free TCP port.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}

	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		return 0, fmt.Errorf("close listener: %w", err)
	}

	return port, nil
}

// GiteaAPI is a minimal Gitea API client for test setup.
type GiteaAPI struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

// NewGiteaAPI creates a test API client. Call CreateToken first to get a token.
func NewGiteaAPI(baseURL string) *GiteaAPI {
	return &GiteaAPI{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Client:  &http.Client{Timeout: 10 * time.Second},
	}
}

// CreateToken creates an API token for the admin user.
func (a *GiteaAPI) CreateToken(t *testing.T) string {
	t.Helper()

	body := `{"name": "test-token", "scopes": ["all"]}`

	req, err := http.NewRequest(http.MethodPost,
		a.BaseURL+"/api/v1/users/testuser/tokens",
		strings.NewReader(body))
	if err != nil {
		t.Fatalf("create token request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("testuser", "testpass123")

	resp, err := a.Client.Do(req)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Warn("failed to close response body", "error", err)
		}
	}()

	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("create token: status %d: %s", resp.StatusCode, respBody)
	}

	var result struct {
		SHA1 string `json:"sha1"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode token response: %v", err)
	}

	a.Token = result.SHA1

	return result.SHA1
}

// Do performs an authenticated API request and returns the response body.
func (a *GiteaAPI) Do(t *testing.T, method, path string, body string) (int, []byte) {
	t.Helper()

	var reqBody io.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	}

	req, err := http.NewRequest(method, a.BaseURL+"/api/v1"+path, reqBody)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	if a.Token != "" {
		req.Header.Set("Authorization", "token "+a.Token)
	}

	resp, err := a.Client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Warn("failed to close response body", "error", err)
		}
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}

	return resp.StatusCode, respBody
}

// MustDo performs an authenticated API request and fatals if the status is not 2xx.
func (a *GiteaAPI) MustDo(t *testing.T, method, path string, body string) []byte {
	t.Helper()

	status, respBody := a.Do(t, method, path, body)
	if status < 200 || status >= 300 {
		t.Fatalf("%s %s: status %d: %s", method, path, status, respBody)
	}

	return respBody
}

//nolint:gochecknoglobals
var globalGitea *GiteaServer

// SetGiteaServer stores the shared Gitea server for test helpers.
func SetGiteaServer(s *GiteaServer) {
	globalGitea = s
}

// GiteaInstance returns the shared Gitea server started by RunWithGitea.
func GiteaInstance() *GiteaServer {
	return globalGitea
}

// RunWithPostgresAndGitea is a helper for TestMain: starts postgres and Gitea,
// runs tests, cleans up. Returns the exit code for os.Exit.
// If gitea is not in PATH, Gitea is skipped and tests that need it will
// be skipped via GiteaInstance() returning nil.
func RunWithPostgresAndGitea(m *testing.M) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_ = os.Unsetenv("PGDATABASE")
	_ = os.Unsetenv("PGUSER")
	_ = os.Unsetenv("PGHOST")

	pgServer, err := StartPostgresServer(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start postgres: %v\n", err)

		return 1
	}

	defer pgServer.Cleanup()

	globalServer = pgServer

	if _, lookErr := exec.LookPath("gitea"); lookErr != nil {
		fmt.Fprintf(os.Stderr, "gitea not in PATH, skipping Gitea integration tests\n")
	} else {
		giteaServer, giteaErr := StartGiteaServer(ctx)
		if giteaErr != nil {
			fmt.Fprintf(os.Stderr, "failed to start gitea (tests needing it will be skipped): %v\n", giteaErr)
		} else {
			defer giteaServer.Cleanup()

			globalGitea = giteaServer
		}
	}

	return m.Run()
}
