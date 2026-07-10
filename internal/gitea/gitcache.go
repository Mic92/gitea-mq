package gitea

// Persistent per-repository git cache. One bare, blobless clone per repo,
// fetched incrementally instead of re-cloned per operation. Merges happen
// in-memory (merge-tree/commit-tree), so no worktree is ever created.
//
// Every git command runs through one runner that injects the API token as an
// http.<url>.extraHeader -c option, so pushes, fetches and lazy promisor blob
// fetches are all authenticated and the token never hits disk.
//
// The cache is disposable: corrupted clones are deleted and rebuilt, and a
// .last-used marker drives age-based cleanup of repos removed from config.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const cacheMarkerFile = ".last-used"

// DefaultCacheMaxAge is how long an unused cached repository is kept.
const DefaultCacheMaxAge = 30 * 24 * time.Hour

type gitCache struct {
	baseDir   string
	authFlags []string            // git -c options prepended to every command
	redact    func(string) string // strips secrets from git output

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newGitCache(baseDir string, authFlags []string, redact func(string) string) *gitCache {
	return &gitCache{
		baseDir:   baseDir,
		authFlags: authFlags,
		redact:    redact,
		locks:     make(map[string]*sync.Mutex),
	}
}

func (g *gitCache) repoLock(key string) *sync.Mutex {
	g.mu.Lock()
	defer g.mu.Unlock()
	l, ok := g.locks[key]
	if !ok {
		l = &sync.Mutex{}
		g.locks[key] = l
	}
	return l
}

// repoPath includes a URL hash so same-named repos on different servers don't collide.
func (g *gitCache) repoPath(url, owner, repo string) string {
	sum := sha256.Sum256([]byte(url))
	return filepath.Join(g.baseDir, owner, fmt.Sprintf("%s-%x.git", repo, sum[:4]))
}

func (g *gitCache) runner(ctx context.Context, dir string) func(args ...string) (string, error) {
	return func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append(append([]string{}, g.authFlags...), args...)...)
		cmd.Dir = dir
		cmd.Env = append(
			os.Environ(),
			"GIT_TERMINAL_PROMPT=0",
			"GIT_AUTHOR_NAME=gitea-mq", "GIT_AUTHOR_EMAIL=gitea-mq@localhost",
			"GIT_COMMITTER_NAME=gitea-mq", "GIT_COMMITTER_EMAIL=gitea-mq@localhost",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return string(out), fmt.Errorf("git %s: %w\n%s",
				g.redact(strings.Join(args, " ")), err, g.redact(string(out)))
		}
		return string(out), nil
	}
}

// withRepo fetches refs (branch refspecs or SHAs) into the cached clone and
// runs fn with a runner bound to it. The per-repo lock is held for the whole
// operation so gc or a corruption re-clone can't remove objects under fn.
func (g *gitCache) withRepo(ctx context.Context, url, owner, repo string, refs []string, fn func(run func(args ...string) (string, error)) error) error {
	l := g.repoLock(owner + "/" + repo)
	l.Lock()
	defer l.Unlock()

	path := g.repoPath(url, owner, repo)
	if err := g.ensure(ctx, path, url, refs); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(path, cacheMarkerFile), nil, 0o644); err != nil {
		slog.Warn("git cache: write last-used marker", "path", path, "err", err)
	}

	if err := fn(g.runner(ctx, path)); err != nil {
		return err
	}
	if _, err := g.runner(ctx, path)("gc", "--auto", "--quiet"); err != nil {
		slog.Debug("git cache: gc --auto failed", "path", path, "err", err)
	}
	return nil
}

// ensure fetches refs, rebuilding the clone if it is corrupted. A healthy
// clone means the failure was transient (network, auth) and is surfaced.
func (g *gitCache) ensure(ctx context.Context, path, url string, refs []string) error {
	err := g.fetch(ctx, path, url, refs)
	if err == nil || g.healthy(ctx, path) {
		return err
	}
	slog.Warn("git cache corrupted, re-creating", "path", path, "err", err)
	if rmErr := os.RemoveAll(path); rmErr != nil {
		return fmt.Errorf("remove corrupted cache %s: %w", path, rmErr)
	}
	return g.fetch(ctx, path, url, refs)
}

func (g *gitCache) fetch(ctx context.Context, path, url string, refs []string) error {
	run := g.runner(ctx, path)
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err != nil {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return fmt.Errorf("create cache dir: %w", err)
		}
		// Promisor remote lets git backfill blobs lazily whenever a later
		// command (merge-tree, cat-file, ...) needs file contents.
		for _, args := range [][]string{
			{"init", "--bare", "--quiet"},
			{"remote", "add", "origin", url},
			{"config", "remote.origin.promisor", "true"},
			{"config", "remote.origin.partialclonefilter", "blob:none"},
		} {
			if _, err := run(args...); err != nil {
				return fmt.Errorf("init cache: %w", err)
			}
		}
	}
	// Keep the remote URL current in case the base URL changed.
	if _, err := run("remote", "set-url", "origin", url); err != nil {
		return fmt.Errorf("configure cache: %w", err)
	}
	args := append([]string{"fetch", "--quiet", "--force", "--prune", "--filter=blob:none", "origin"}, refs...)
	if _, err := run(args...); err != nil {
		return fmt.Errorf("fetch into cache: %w", err)
	}
	return nil
}

// healthy is a cheap corruption check used only after a fetch failure.
func (g *gitCache) healthy(ctx context.Context, path string) bool {
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err != nil {
		return false
	}
	run := g.runner(ctx, path)
	out, err := run("for-each-ref", "--count=1", "--format=%(objectname)", "refs/heads")
	if err != nil {
		return false
	}
	if sha := strings.TrimSpace(out); sha != "" {
		if _, err := run("cat-file", "-e", sha+"^{commit}"); err != nil {
			return false
		}
	}
	return true
}

// cleanupStale removes cached repos not used within maxAge; this is how
// caches of repos removed from the configuration go away. Call at startup.
func (g *gitCache) cleanupStale(maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)
	dirs, err := filepath.Glob(filepath.Join(g.baseDir, "*", "*.git"))
	if err != nil {
		return
	}
	for _, dir := range dirs {
		info, err := os.Stat(filepath.Join(dir, cacheMarkerFile))
		if err != nil {
			if info, err = os.Stat(dir); err != nil {
				continue
			}
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		slog.Info("git cache: removing stale repository", "path", dir)
		if err := os.RemoveAll(dir); err != nil {
			slog.Warn("git cache: remove stale repository", "path", dir, "err", err)
		}
	}
}
