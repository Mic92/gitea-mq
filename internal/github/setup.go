package github

import (
	"context"
	"log/slog"
	"net/http"

	gh "github.com/google/go-github/v84/github"

	"github.com/Mic92/gitea-mq/internal/forge"
)

// GitHub's built-in repository role ID for "admin".
const repoAdminRoleID int64 = 5

func (f *githubForge) EnsureRepoSetup(ctx context.Context, owner, name string, _ forge.SetupConfig) error {
	c, err := f.app.ClientForRepo(owner, name)
	if err != nil {
		return err
	}

	// Auto-merge is the user signal for "queue this PR"; without it the
	// poller never enqueues anything.
	if _, resp, err := c.Repositories.Edit(ctx, owner, name, &gh.Repository{
		AllowAutoMerge: gh.Ptr(true),
	}); err != nil {
		if !isForbidden(resp) {
			return err
		}
		slog.Warn("github: cannot enable allow_auto_merge (App lacks Administration permission)",
			"repo", owner+"/"+name, "err", err)
	}

	rss, resp, err := c.Repositories.GetAllRulesets(ctx, owner, name, nil)
	if err != nil {
		if !isForbidden(resp) {
			return err
		}
		slog.Warn("github: cannot manage rulesets (App lacks Administration permission)",
			"repo", owner+"/"+name, "err", err)
		return nil
	}
	for _, rs := range rss {
		if rs.Name == forge.MQContext {
			return nil
		}
	}

	_, resp, err = c.Repositories.CreateRuleset(ctx, owner, name, gh.RepositoryRuleset{
		Name:        forge.MQContext,
		Target:      gh.Ptr(gh.RulesetTargetBranch),
		Enforcement: gh.RulesetEnforcementActive,
		BypassActors: []*gh.BypassActor{
			// The App must bypass its own gate to manage merge branches and
			// to let GitHub fast-forward when it reports success.
			{
				ActorID:    gh.Ptr(f.app.AppID()),
				ActorType:  gh.Ptr(gh.BypassActorTypeIntegration),
				BypassMode: gh.Ptr(gh.BypassModeAlways),
			},
			// Repo admins keep an escape hatch for hotfixes.
			{
				ActorID:    gh.Ptr(repoAdminRoleID),
				ActorType:  gh.Ptr(gh.BypassActorTypeRepositoryRole),
				BypassMode: gh.Ptr(gh.BypassModeAlways),
			},
		},
		Conditions: &gh.RepositoryRulesetConditions{
			RefName: &gh.RepositoryRulesetRefConditionParameters{
				// ~ALL would also gate every feature-branch push on a check
				// that only ever reports for queued PRs.
				Include: []string{"~DEFAULT_BRANCH"},
				Exclude: []string{},
			},
		},
		Rules: &gh.RepositoryRulesetRules{
			RequiredStatusChecks: &gh.RequiredStatusChecksRuleParameters{
				RequiredStatusChecks: []*gh.RuleStatusCheck{{
					Context:       forge.MQContext,
					IntegrationID: gh.Ptr(f.app.AppID()),
				}},
				StrictRequiredStatusChecksPolicy: false,
				// Otherwise the rule also gates branch *creation* and only
				// the bypass actor could push a new branch.
				DoNotEnforceOnCreate: gh.Ptr(true),
			},
		},
	})
	if err != nil {
		if !isForbidden(resp) {
			return err
		}
		slog.Warn("github: cannot create ruleset (App lacks Administration permission)",
			"repo", owner+"/"+name, "err", err)
	}
	return nil
}

func isForbidden(resp *gh.Response) bool {
	return resp != nil && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound)
}
