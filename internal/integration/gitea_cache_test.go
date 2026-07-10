package integration_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Mic92/gitea-mq/internal/gitea"
	"github.com/Mic92/gitea-mq/internal/testutil"
)

// TestMergeBranches_PrivateRepo_CachedClone is the regression test for the
// blobless-cache credential trap: with a private repo, every git operation —
// including lazy promisor blob fetches during merge-tree and the push — must
// be authenticated. It runs MergeBranches twice against the same persistent
// cache so the second run exercises the incremental-fetch path.
func TestMergeBranches_PrivateRepo_CachedClone(t *testing.T) {
	gs := testutil.GiteaInstance()
	if gs == nil {
		t.Skip("gitea server not available")
	}
	api := testutil.NewGiteaAPI(gs.URL)
	api.CreateToken(t)
	c := gitea.NewHTTPClient(gs.URL, api.Token)
	c.SetGitCacheDir(t.TempDir())
	ctx := t.Context()

	repo := "cache-private-test"
	api.MustDo(t, "POST", "/user/repos", `{"name": "`+repo+`", "auto_init": false, "default_branch": "main", "private": true}`)
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
		return string(out)
	}
	write := func(name, body string) { _ = os.WriteFile(filepath.Join(tmp, name), []byte(body), 0o644) }

	run("init", "-q", "-b", "main")
	write("f", "base\n")
	run("add", ".")
	run("commit", "-q", "-m", "base")
	run("push", "-q", url, "main")

	head := func(branch, file, body string) string {
		run("checkout", "-q", "-b", branch, "main")
		write(file, body)
		run("add", ".")
		run("commit", "-q", "-m", branch)
		run("push", "-q", url, branch)
		run("checkout", "-q", "main")
		return run("rev-parse", branch)[:40]
	}
	h1 := head("p1", "a", "a\n")

	res, err := c.MergeBranches(ctx, "testuser", repo, "main", h1, "gitea-mq/one")
	if err != nil {
		t.Fatalf("first MergeBranches: %v", err)
	}
	if res.SHA == "" {
		t.Fatal("empty merge SHA")
	}

	// Second merge reuses the cached clone: incremental fetch plus lazy blob
	// fetch of a file the first merge never touched.
	h2 := head("p2", "f", "changed\n")
	if _, err := c.MergeBranches(ctx, "testuser", repo, "main", h2, "gitea-mq/two"); err != nil {
		t.Fatalf("second MergeBranches: %v", err)
	}
}
