package github

import (
	"context"
	"sync"

	gh "github.com/google/go-github/v84/github"

	"github.com/Mic92/gitea-mq/internal/forge"
	"github.com/Mic92/gitea-mq/internal/store/pg"
)

// checkRunCache remembers (repo, sha, name) → check-run ID so SetMQStatus and
// MirrorCheck PATCH the existing run instead of creating duplicates. The cache
// is best-effort: on miss it lists check runs by name and falls back to create.
type checkRunCache struct {
	mu  sync.Mutex
	ids map[[3]string]int64 // owner/name, sha, checkName
}

func (c *checkRunCache) get(repo, sha, name string) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id, ok := c.ids[[3]string{repo, sha, name}]
	return id, ok
}

func (c *checkRunCache) set(repo, sha, name string, id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ids == nil {
		c.ids = map[[3]string]int64{}
	}
	c.ids[[3]string{repo, sha, name}] = id
}

// mqStateToCheckRun maps the queue lifecycle onto GitHub check-run fields.
// "error" surfaces as cancelled rather than failure so dashboards distinguish
// infrastructure problems from CI verdicts.
func mqStateToCheckRun(state forge.CheckState) (status, conclusion string) {
	switch state {
	case pg.CheckStatePending:
		return "in_progress", ""
	case pg.CheckStateSuccess:
		return "completed", "success"
	case pg.CheckStateFailure:
		return "completed", "failure"
	case pg.CheckStateError:
		return "completed", "cancelled"
	default:
		return "queued", ""
	}
}

// rawStateToCheckRun maps a forge.MirrorCheck state string. It accepts the
// "skipped" sentinel monitor uses for stale-mirror cleanup.
func rawStateToCheckRun(state string) (status, conclusion string) {
	switch state {
	case "pending":
		return "in_progress", ""
	case "success":
		return "completed", "success"
	case "failure":
		return "completed", "failure"
	case "error":
		return "completed", "cancelled"
	case "skipped":
		return "completed", "skipped"
	default:
		return "queued", ""
	}
}

// CheckRunToState maps a GitHub check run back to a forge.CheckState.
// Unfinished runs are pending; neutral/skipped count as success so they do
// not block the queue.
func CheckRunToState(status, conclusion string) forge.CheckState {
	if status != "completed" {
		return pg.CheckStatePending
	}
	switch conclusion {
	case "success", "neutral", "skipped":
		return pg.CheckStateSuccess
	case "failure", "timed_out", "action_required":
		return pg.CheckStateFailure
	default: // cancelled, stale
		return pg.CheckStateError
	}
}

func (f *githubForge) upsertCheckRun(ctx context.Context, owner, name, sha, checkName, status, conclusion, summary, detailsURL string) error {
	c, err := f.app.ClientForRepo(owner, name)
	if err != nil {
		return err
	}
	repoKey := owner + "/" + name

	id, cached := f.checkRuns.get(repoKey, sha, checkName)
	if !cached {
		// Cold path after restart: look up by name so we PATCH instead of
		// piling up duplicate runs on the same SHA.
		runs, _, err := c.Checks.ListCheckRunsForRef(ctx, owner, name, sha,
			&gh.ListCheckRunsOptions{CheckName: gh.Ptr(checkName)})
		if err != nil {
			return err
		}
		if len(runs.CheckRuns) > 0 {
			id = runs.CheckRuns[0].GetID()
			cached = true
		}
	}

	output := &gh.CheckRunOutput{Title: gh.Ptr(checkName), Summary: gh.Ptr(summary)}
	var conclP *string
	if conclusion != "" {
		conclP = gh.Ptr(conclusion)
	}
	var urlP *string
	if detailsURL != "" {
		urlP = gh.Ptr(detailsURL)
	}

	if cached {
		_, _, err = c.Checks.UpdateCheckRun(ctx, owner, name, id, gh.UpdateCheckRunOptions{
			Name: checkName, Status: gh.Ptr(status), Conclusion: conclP,
			DetailsURL: urlP, Output: output,
		})
		if err == nil {
			f.checkRuns.set(repoKey, sha, checkName, id)
		}
		return err
	}

	cr, _, err := c.Checks.CreateCheckRun(ctx, owner, name, gh.CreateCheckRunOptions{
		Name: checkName, HeadSHA: sha, Status: gh.Ptr(status), Conclusion: conclP,
		DetailsURL: urlP, Output: output,
	})
	if err != nil {
		return err
	}
	f.checkRuns.set(repoKey, sha, checkName, cr.GetID())
	return nil
}
