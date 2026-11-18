package gitea

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The token must reach git's http.<url>.extraheader without appearing in
// git's argv, which other local users can read from /proc/<pid>/cmdline.
func TestGitAuthHeaderStaysOutOfArgv(t *testing.T) {
	const token = "test-token-not-a-real-credential"
	client := NewHTTPClient("http://gitea.invalid", token)
	want := "Authorization: token " + token

	// Real git resolves the header from the environment reference.
	got, err := client.gitCache.runner(context.Background(), t.TempDir())(
		"config", "--get", "http.http://gitea.invalid/.extraheader")
	if err != nil {
		t.Fatalf("git config: %v", err)
	}
	if strings.TrimSpace(got) != want {
		t.Fatalf("extraheader = %q, want the Authorization header", strings.TrimSpace(got))
	}

	if runtime.GOOS == "windows" {
		t.Skip("argv capture uses a POSIX shell stub")
	}
	// A stub git records its argv and the env value it received.
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	stub := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$GITAUTH_TEST_ARGS\"\nprintf '%s' \"$" + gitAuthHeaderEnv + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GITAUTH_TEST_ARGS", argsFile)

	got, err = client.gitCache.runner(context.Background(), dir)("rev-parse", "--git-dir")
	if err != nil {
		t.Fatalf("stub git: %v", err)
	}
	if got != want {
		t.Fatalf("child environment header = %q, want the Authorization header", got)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), token) {
		t.Fatal("token appeared in git argv")
	}
	if !strings.Contains(string(args), "--config-env=http.http://gitea.invalid/.extraheader="+gitAuthHeaderEnv) {
		t.Fatalf("argv lacks the --config-env reference: %q", args)
	}
}

func TestGitAuthWithoutTokenAddsNothing(t *testing.T) {
	client := NewHTTPClient("http://gitea.invalid", "")
	if len(client.gitCache.authFlags) != 0 || len(client.gitCache.authEnv) != 0 {
		t.Fatal("no token should add no git auth configuration")
	}
}
