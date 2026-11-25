package metrics_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Mic92/gitea-mq/internal/forge"
	"github.com/Mic92/gitea-mq/internal/metrics"
	"github.com/Mic92/gitea-mq/internal/store/pg"
	"github.com/Mic92/gitea-mq/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.RunWithPostgres(m))
}

type repoList []forge.RepoRef

func (r repoList) List() []forge.RepoRef { return r }

func scrape(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	body, _ := io.ReadAll(rec.Body)
	return string(body)
}

func assertLines(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w+"\n") && !strings.Contains(out, w+" ") {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
}

func TestScrapeReportsQueueStatePerBranch(t *testing.T) {
	svc, ctx, repoID := testutil.TestQueueService(t)

	for _, pr := range []int64{1, 2, 3} {
		if _, err := svc.Enqueue(ctx, repoID, pr, "sha", "main", false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.Enqueue(ctx, repoID, 4, "sha", "release", false); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateState(ctx, repoID, 1, pg.EntryStateTesting); err != nil {
		t.Fatal(err)
	}

	h := metrics.Handler(svc, repoList{
		{Forge: forge.KindGitea, Owner: "org", Name: "app"},
		{Forge: forge.KindGithub, Owner: "org", Name: "idle"},
	})
	out := scrape(t, h)
	assertLines(t, out,
		`gitea_mq_managed_repos 2`,
		`gitea_mq_queue_entries{branch="main",forge="gitea",owner="org",repo="app",state="queued"} 2`,
		`gitea_mq_queue_entries{branch="main",forge="gitea",owner="org",repo="app",state="testing"} 1`,
		`gitea_mq_queue_entries{branch="main",forge="gitea",owner="org",repo="app",state="success"} 0`,
		`gitea_mq_queue_entries{branch="release",forge="gitea",owner="org",repo="app",state="queued"} 1`,
		`gitea_mq_live_batches{branch="release",forge="gitea",owner="org",repo="app",state="testing"} 0`,
		`gitea_mq_queue_oldest_entry_timestamp_seconds{branch="release",forge="gitea",owner="org",repo="app"}`,
	)
	if strings.Contains(out, `repo="idle"`) {
		t.Errorf("repo without any branch history must not report branch series:\n%s", out)
	}

	// A drained branch keeps reporting 0 rather than vanishing.
	if err := svc.Dequeue(ctx, repoID, 4); err != nil {
		t.Fatal(err)
	}
	out = scrape(t, h)
	assertLines(t, out,
		`gitea_mq_queue_entries{branch="release",forge="gitea",owner="org",repo="app",state="queued"} 0`,
	)
	if strings.Contains(out, `gitea_mq_queue_oldest_entry_timestamp_seconds{branch="release"`) {
		t.Errorf("drained branch must not report an oldest entry:\n%s", out)
	}
}
