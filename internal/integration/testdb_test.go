package integration_test

import (
	"os"
	"testing"

	"github.com/Mic92/gitea-mq/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.RunWithPostgresAndGitea(m))
}
