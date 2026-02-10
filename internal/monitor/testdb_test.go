package monitor_test

import (
	"os"
	"testing"

	"github.com/jogman/gitea-mq/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.RunWithPostgres(m))
}
