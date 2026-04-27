package webhook

import (
	"log/slog"
	"net/http"

	gh "github.com/google/go-github/v84/github"

	"github.com/Mic92/gitea-mq/internal/forge"
	"github.com/Mic92/gitea-mq/internal/github"
	"github.com/Mic92/gitea-mq/internal/queue"
)

// prTriggerActions are the pull_request actions that change the desired queue
// state. They all reduce to a poller reconcile because the poller already
// owns the full enqueue/dequeue/retarget logic and is idempotent.
var prTriggerActions = map[string]bool{
	"auto_merge_enabled":  true,
	"auto_merge_disabled": true,
	"closed":              true,
	"reopened":            true,
	"synchronize":         true,
	"edited":              true, // base-branch retarget arrives as edited
}

// GithubHandler validates X-Hub-Signature-256, dispatches by X-GitHub-Event,
// and returns 200 for anything it does not act on so GitHub does not mark the
// delivery as failed.
func GithubHandler(secret []byte, repos RepoLookup, queueSvc *queue.Service, triggerDiscovery func()) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, err := gh.ValidatePayload(r, secret)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		event, err := gh.ParseWebHook(gh.WebHookType(r), payload)
		if err != nil {
			slog.Warn("github webhook: parse failed", "type", gh.WebHookType(r), "err", err)
			w.WriteHeader(http.StatusOK)
			return
		}

		switch e := event.(type) {
		case *gh.PullRequestEvent:
			if !prTriggerActions[e.GetAction()] {
				break
			}
			if rm, ok := lookupGithubRepo(repos, e.GetRepo()); ok && rm.TriggerPoll != nil {
				rm.TriggerPoll()
			}

		case *gh.CheckRunEvent:
			cr := e.GetCheckRun()
			if e.GetAction() != "completed" || cr.GetStatus() != "completed" {
				break
			}
			if forge.IsOwnContext(cr.GetName()) {
				break
			}
			rm, ok := lookupGithubRepo(repos, e.GetRepo())
			if !ok {
				break
			}
			state := github.CheckRunToState(cr.GetStatus(), cr.GetConclusion())
			routeCheck(r.Context(), rm, queueSvc, cr.GetHeadSHA(), cr.GetName(),
				state, string(state), cr.GetOutput().GetSummary(), cr.GetDetailsURL())

		case *gh.StatusEvent:
			if forge.IsOwnContext(e.GetContext()) {
				break
			}
			rm, ok := lookupGithubRepo(repos, e.GetRepo())
			if !ok {
				break
			}
			routeCheck(r.Context(), rm, queueSvc, e.GetSHA(), e.GetContext(),
				forge.ParseCheckState(e.GetState()), e.GetState(), e.GetDescription(), e.GetTargetURL())

		case *gh.InstallationEvent, *gh.InstallationRepositoriesEvent:
			if triggerDiscovery != nil {
				triggerDiscovery()
			}
		}

		w.WriteHeader(http.StatusOK)
	})
}

func lookupGithubRepo(repos RepoLookup, r *gh.Repository) (*RepoMonitor, bool) {
	owner, name := r.GetOwner().GetLogin(), r.GetName()
	if owner == "" || name == "" {
		return nil, false
	}
	return repos.LookupMonitor(string(forge.KindGithub) + ":" + owner + "/" + name)
}
