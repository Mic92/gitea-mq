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
	// The App must bypass its own gate to manage merge branches and to let
	// GitHub fast-forward when it reports success.
	self := &gh.BypassActor{
		ActorID:    gh.Ptr(f.app.AppID()),
		ActorType:  gh.Ptr(gh.BypassActorTypeIntegration),
		BypassMode: gh.Ptr(gh.BypassModeAlways),
	}
	haveMQ := false
	for _, rs := range rss {
		if rs.Name == forge.MQContext {
			haveMQ = true
			continue
		}
		if err := f.ensureBypass(ctx, c, owner, name, rs, self); err != nil {
			return err
		}
	}
	if haveMQ {
		return nil
	}

	_, resp, err = c.Repositories.CreateRuleset(ctx, owner, name, gh.RepositoryRuleset{
		Name:        forge.MQContext,
		Target:      gh.Ptr(gh.RulesetTargetBranch),
		Enforcement: gh.RulesetEnforcementActive,
		BypassActors: []*gh.BypassActor{
			self,
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

// ensureBypass adds the App as bypass actor to a pre-existing branch ruleset
// (e.g. "require pull request") that would otherwise reject the queue's
// fast-forward with "Repository rule violations found".
func (f *githubForge) ensureBypass(ctx context.Context, c *gh.Client, owner, name string, summary *gh.RepositoryRuleset, self *gh.BypassActor) error {
	if summary.Target == nil || *summary.Target != gh.RulesetTargetBranch {
		return nil
	}
	// The list endpoint omits bypass_actors and rules.
	rs, _, err := c.Repositories.GetRuleset(ctx, owner, name, summary.GetID(), false)
	if err != nil {
		return err
	}
	for _, b := range rs.BypassActors {
		if b.ActorType != nil && *b.ActorType == gh.BypassActorTypeIntegration && b.GetActorID() == self.GetActorID() {
			return nil
		}
	}
	if rs.SourceType == nil || *rs.SourceType != gh.RulesetSourceTypeRepository {
		slog.Warn("github: org-level ruleset lacks gitea-mq bypass; fast-forwards to the default branch may be rejected",
			"repo", owner+"/"+name, "ruleset", rs.Name, "source", rs.Source)
		return nil
	}
	rs.BypassActors = append(rs.BypassActors, self)
	if _, resp, err := c.Repositories.UpdateRuleset(ctx, owner, name, rs.GetID(), *rs); err != nil {
		if !isForbidden(resp) {
			return err
		}
		slog.Warn("github: cannot add gitea-mq bypass to ruleset",
			"repo", owner+"/"+name, "ruleset", rs.Name, "err", err)
		return nil
	}
	slog.Info("github: added gitea-mq as ruleset bypass actor", "repo", owner+"/"+name, "ruleset", rs.Name)
	return nil
}

func isForbidden(resp *gh.Response) bool {
	return resp != nil && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound)
}
