package integration_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Mic92/gitea-mq/internal/gitea"
	"github.com/Mic92/gitea-mq/internal/testutil"
)

// TestFastForwardRef_RealGitea verifies that the git-binary push approach
// works against a live Gitea: a depth=1 fetch of the to-be-pushed SHA is
// enough for a pure ref update (the server already has every object), and a
// non-fast-forward push is reported as such.
func TestFastForwardRef_RealGitea(t *testing.T) {
	gs := testutil.GiteaInstance()
	if gs == nil {
		t.Skip("gitea server not available")
	}
	api := testutil.NewGiteaAPI(gs.URL)
	api.CreateToken(t)
	c := gitea.NewHTTPClient(gs.URL, api.Token)
	ctx := t.Context()

	repo := "ff-test"
	api.MustDo(t, "POST", "/user/repos", `{"name": "`+repo+`", "auto_init": false, "default_branch": "main"}`)
	if err := gs.PatchRepoHooks("testuser", repo); err != nil {
		t.Fatalf("patch hooks: %v", err)
	}

	// Seed history locally and push so we control SHAs precisely:
	//   main:       A
	//   batch:      A──B──C
	tmp := t.TempDir()
	url := fmt.Sprintf("%s://gitea-mq:%s@%s/testuser/%s.git", "http", api.Token, gs.URL[len("http://"):], repo)
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = tmp
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	run("init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(tmp, "a"), []byte("a"), 0o644)
	run("add", ".")
	run("commit", "-q", "-m", "A")
	run("push", "-q", url, "main")
	run("checkout", "-q", "-b", "batch")
	for _, f := range []string{"b", "c"} {
		_ = os.WriteFile(filepath.Join(tmp, f), []byte(f), 0o644)
		run("add", ".")
		run("commit", "-q", "-m", f)
	}
	run("push", "-q", url, "batch")
	shaC := run("rev-parse", "HEAD")[:40]
	run("checkout", "-q", "main")
	shaA := run("rev-parse", "HEAD")[:40]

	// Happy path: main A → C is a fast-forward.
	if err := c.FastForwardRef(ctx, "testuser", repo, "main", shaC); err != nil {
		t.Fatalf("fast-forward A→C: %v", err)
	}

	// Non-ff: main is now at C; pushing A is a rewind. The git-level ref is
	// synchronous even though Gitea's /branches API lags the post-receive
	// hook, so the server-side non-ff check is reliable here.
	err := c.FastForwardRef(ctx, "testuser", repo, "main", shaA)
	var nff *gitea.NotFastForwardError
	if !errors.As(err, &nff) {
		t.Fatalf("rewind C→A: got %v, want NotFastForwardError", err)
	}
}

// TestStackMerges_RealGitea verifies the one-clone stacker against a live
// Gitea: two clean PR heads merge, a conflicting one is reported and skipped,
// and the resulting branch contains both clean changes.
func TestStackMerges_RealGitea(t *testing.T) {
	gs := testutil.GiteaInstance()
	if gs == nil {
		t.Skip("gitea server not available")
	}
	api := testutil.NewGiteaAPI(gs.URL)
	api.CreateToken(t)
	c := gitea.NewHTTPClient(gs.URL, api.Token)
	ctx := t.Context()

	repo := "stack-test"
	api.MustDo(t, "POST", "/user/repos", `{"name": "`+repo+`", "auto_init": false, "default_branch": "main"}`)
	if err := gs.PatchRepoHooks("testuser", repo); err != nil {
		t.Fatalf("patch hooks: %v", err)
	}

	tmp := t.TempDir()
	url := fmt.Sprintf("http://gitea-mq:%s@%s/testuser/%s.git", api.Token, gs.URL[len("http://"):], repo)
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = tmp
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
	write := func(name, body string) { _ = os.WriteFile(filepath.Join(tmp, name), []byte(body), 0o644) }

	run("init", "-q", "-b", "main")
	write("f", "base\n")
	run("add", ".")
	run("commit", "-q", "-m", "base")
	run("push", "-q", url, "main")

	headOf := func(branch, file, body string) string {
		run("checkout", "-q", "-b", branch, "main")
		write(file, body)
		run("add", ".")
		run("commit", "-q", "-m", branch)
		run("push", "-q", url, branch)
		return run("rev-parse", "HEAD")
	}
	h1 := headOf("p1", "a", "a\n")
	h2 := headOf("p2", "f", "theirs\n")
	h3 := headOf("p3", "b", "b\n")

	// Advance main so p2's edit of `f` conflicts (both sides changed).
	run("checkout", "-q", "main")
	write("f", "ours\n")
	run("add", ".")
	run("commit", "-q", "-m", "main edits f")
	run("push", "-q", url, "main")

	tip, steps, err := c.StackMerges(ctx, "testuser", repo, "main", []string{h1, h2, h3}, "gitea-mq/batch/1")
	if err != nil {
		t.Fatalf("StackMerges: %v", err)
	}
	if len(steps) != 3 || steps[0].Conflict || !steps[1].Conflict || steps[2].Conflict {
		t.Fatalf("steps = %+v", steps)
	}
	if tip == "" {
		t.Fatal("tip empty")
	}

	// Batch branch must contain p1 and p3 changes, and the main-side `f`.
	run("fetch", "-q", url, "gitea-mq/batch/1")
	run("checkout", "-q", "FETCH_HEAD")
	for name, want := range map[string]string{"a": "a\n", "b": "b\n", "f": "ours\n"} {
		got, _ := os.ReadFile(filepath.Join(tmp, name))
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
}
