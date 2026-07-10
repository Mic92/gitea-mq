package gitea

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitIn runs git in dir with a fixed identity and fatals on failure.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newOriginRepo creates a bare "server" repo plus a working clone to commit
// through, and returns (originPath, workPath).
func newOriginRepo(t *testing.T) (string, string) {
	t.Helper()
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitIn(t, t.TempDir(), "init", "--bare", "-b", "main", origin)
	// Local transport needs this for --filter=blob:none fetches.
	gitIn(t, origin, "config", "uploadpack.allowFilter", "true")
	work := t.TempDir()
	gitIn(t, work, "init", "-q", "-b", "main")
	gitIn(t, work, "remote", "add", "origin", origin)
	commitFile(t, work, "f", "base\n", "base")
	gitIn(t, work, "push", "-q", "origin", "main")
	return origin, work
}

func commitFile(t *testing.T, work, name, content, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(work, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, work, "add", ".")
	gitIn(t, work, "commit", "-q", "-m", msg)
	return gitIn(t, work, "rev-parse", "HEAD")
}

func newTestCache(t *testing.T) *gitCache {
	t.Helper()
	return newGitCache(t.TempDir(), nil, func(s string) string { return s })
}

// TestGitCache_IncrementalFetch verifies the clone persists across operations
// and later fetches pick up new commits without re-cloning.
func TestGitCache_IncrementalFetch(t *testing.T) {
	origin, work := newOriginRepo(t)
	g := newTestCache(t)
	ctx := context.Background()
	refs := []string{"+refs/heads/main:refs/heads/main"}

	if err := g.withRepo(ctx, origin, "o", "r", refs, func(run gitRunFunc) error {
		_, err := run("rev-parse", "refs/heads/main")
		return err
	}); err != nil {
		t.Fatalf("first withRepo: %v", err)
	}

	sha2 := commitFile(t, work, "g", "more\n", "second")
	gitIn(t, work, "push", "-q", "origin", "main")

	if err := g.withRepo(ctx, origin, "o", "r", refs, func(run gitRunFunc) error {
		out, err := run("rev-parse", "refs/heads/main")
		if err != nil {
			return err
		}
		if got := strings.TrimSpace(out); got != sha2 {
			t.Errorf("main = %s, want %s", got, sha2)
		}
		return nil
	}); err != nil {
		t.Fatalf("second withRepo: %v", err)
	}
}

// TestGitCache_CorruptionRecreates verifies a corrupted cache is rebuilt
// instead of failing the operation.
func TestGitCache_CorruptionRecreates(t *testing.T) {
	origin, _ := newOriginRepo(t)
	g := newTestCache(t)
	ctx := context.Background()
	refs := []string{"+refs/heads/main:refs/heads/main"}

	if err := g.withRepo(ctx, origin, "o", "r", refs, func(gitRunFunc) error { return nil }); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	// Truncate every object file: refs still resolve but nothing is readable.
	path := g.repoPath(origin, "o", "r")
	err := filepath.Walk(filepath.Join(path, "objects"), func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		// Pack files are written read-only.
		if err := os.Chmod(p, 0o644); err != nil {
			return err
		}
		return os.Truncate(p, 0)
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := g.withRepo(ctx, origin, "o", "r", refs, func(run gitRunFunc) error {
		_, err := run("cat-file", "-e", "refs/heads/main^{commit}")
		return err
	}); err != nil {
		t.Fatalf("withRepo after corruption: %v", err)
	}
}

// TestGitCache_CleanupStale verifies unused cache repos are removed and
// recently used ones kept.
func TestGitCache_CleanupStale(t *testing.T) {
	origin, _ := newOriginRepo(t)
	g := newTestCache(t)
	ctx := context.Background()
	refs := []string{"+refs/heads/main:refs/heads/main"}

	if err := g.withRepo(ctx, origin, "o", "fresh", refs, func(gitRunFunc) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := g.withRepo(ctx, origin, "o", "stale", refs, func(gitRunFunc) error { return nil }); err != nil {
		t.Fatal(err)
	}
	stalePath := g.repoPath(origin, "o", "stale")
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(stalePath, cacheMarkerFile), old, old); err != nil {
		t.Fatal(err)
	}

	g.cleanupStale(24 * time.Hour)

	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Errorf("stale cache still present: %v", err)
	}
	if _, err := os.Stat(g.repoPath(origin, "o", "fresh")); err != nil {
		t.Errorf("fresh cache removed: %v", err)
	}
}

// TestMergeCommit exercises the in-memory merge: clean merge produces a
// two-parent commit, conflicting branches report a conflict.
func TestMergeCommit(t *testing.T) {
	origin, work := newOriginRepo(t)

	branchHead := func(branch, file, content string) string {
		gitIn(t, work, "checkout", "-q", "-b", branch, "main")
		sha := commitFile(t, work, file, content, branch)
		gitIn(t, work, "push", "-q", "origin", branch)
		gitIn(t, work, "checkout", "-q", "main")
		return sha
	}
	clean := branchHead("clean", "other", "x\n")
	conflict := branchHead("conflict", "f", "theirs\n")
	// Advance main so "conflict" collides on file f.
	commitFile(t, work, "f", "ours\n", "main edit")
	gitIn(t, work, "push", "-q", "origin", "main")

	g := newTestCache(t)
	refs := []string{"+refs/heads/main:refs/heads/main", clean, conflict}
	err := g.withRepo(context.Background(), origin, "o", "r", refs, func(run gitRunFunc) error {
		sha, _, err := mergeCommit(run, "refs/heads/main", clean, "merge clean")
		if err != nil || sha == "" {
			t.Fatalf("clean merge: sha=%q err=%v", sha, err)
		}
		if parents, _ := run("rev-list", "--parents", "-1", sha); len(strings.Fields(parents)) != 3 {
			t.Errorf("expected 2-parent merge commit, got %q", parents)
		}

		sha, out, err := mergeCommit(run, "refs/heads/main", conflict, "merge conflict")
		if err != nil {
			t.Fatalf("conflict merge errored: %v", err)
		}
		if sha != "" || !strings.Contains(out, "f") {
			t.Errorf("expected conflict on f, got sha=%q out=%q", sha, out)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
