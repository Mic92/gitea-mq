package integration_test

import (
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jogman/gitea-mq/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.RunWithPostgres(m))
}

func newTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	return testutil.NewTestDB(t, testutil.Server())
}
