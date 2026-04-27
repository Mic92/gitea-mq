package gitea

import (
	"context"
	"fmt"
	"strings"

	"github.com/Mic92/gitea-mq/internal/forge"
)

// giteaForge owns the Gitea-specific transforms (timeline → AutoMergeEnabled,
// MergeConflictError → conflict bool, self-status filtering) so the rest of
// the system stays forge-agnostic.
type giteaForge struct {
	client  Client
	baseURL string
}

// NewForge wraps a Gitea Client as a forge.Forge. baseURL is the Gitea
// instance root (no trailing /api/v1), used for HTML URL construction.
func NewForge(client Client, baseURL string) forge.Forge {
	return &giteaForge{
		client:  client,
		baseURL: strings.TrimRight(baseURL, "/"),
	}
}

var _ forge.Forge = (*giteaForge)(nil)

func (f *giteaForge) Kind() forge.Kind { return forge.KindGitea }

func (f *giteaForge) RepoHTMLURL(owner, name string) string {
	return f.baseURL + "/" + owner + "/" + name
}

func (f *giteaForge) PRHTMLURL(owner, name string, number int64) string {
	return fmt.Sprintf("%s/%s/%s/pulls/%d", f.baseURL, owner, name, number)
}

func toForgePR(pr *PR, autoMerge bool) forge.PR {
	out := forge.PR{
		Number:           pr.Index,
		Title:            pr.Title,
		Body:             pr.Body,
		State:            pr.State,
		Merged:           pr.HasMerged,
		HTMLURL:          pr.HTMLURL,
		AutoMergeEnabled: autoMerge,
	}
	if pr.User != nil {
		out.AuthorLogin = pr.User.Login
	}
	if pr.Head != nil {
		out.HeadBranch = pr.Head.Ref
		out.HeadSHA = pr.Head.Sha
	}
	if pr.Base != nil {
		out.BaseBranch = pr.Base.Ref
	}
	return out
}

func (f *giteaForge) ListOpenPRs(ctx context.Context, owner, name string) ([]forge.PR, error) {
	prs, err := f.client.ListOpenPRs(ctx, owner, name)
	if err != nil {
		return nil, err
	}
	out := make([]forge.PR, 0, len(prs))
	for i := range prs {
		out = append(out, toForgePR(&prs[i], false))
	}
	return out, nil
}

func (f *giteaForge) GetPR(ctx context.Context, owner, name string, number int64) (*forge.PR, error) {
	pr, err := f.client.GetPR(ctx, owner, name, number)
	if err != nil {
		return nil, err
	}
	timeline, err := f.client.GetPRTimeline(ctx, owner, name, number)
	if err != nil {
		return nil, err
	}
	fp := toForgePR(pr, HasAutomergeScheduled(timeline))
	return &fp, nil
}

// ListAutoMergePRs hides Gitea's timeline-comment signalling: callers see
// only PRs whose latest automerge-related event is "scheduled".
func (f *giteaForge) ListAutoMergePRs(ctx context.Context, owner, name string) ([]forge.PR, error) {
	prs, err := f.client.ListOpenPRs(ctx, owner, name)
	if err != nil {
		return nil, err
	}
	var out []forge.PR
	for i := range prs {
		timeline, err := f.client.GetPRTimeline(ctx, owner, name, prs[i].Index)
		if err != nil {
			return nil, fmt.Errorf("get timeline for PR #%d: %w", prs[i].Index, err)
		}
		if !HasAutomergeScheduled(timeline) {
			continue
		}
		out = append(out, toForgePR(&prs[i], true))
	}
	return out, nil
}

func (f *giteaForge) SetMQStatus(ctx context.Context, owner, name, sha string, st forge.MQStatus) error {
	return f.client.CreateCommitStatus(ctx, owner, name, sha,
		MQStatus(string(st.State), st.Description, st.TargetURL))
}

func (f *giteaForge) GetRequiredChecks(ctx context.Context, owner, name, branch string) ([]string, error) {
	bp, err := f.client.GetBranchProtection(ctx, owner, name, branch)
	if err != nil {
		return nil, err
	}
	if bp == nil || !bp.EnableStatusCheck {
		return nil, nil
	}
	// Never report ourselves as a required external check.
	var out []string
	for _, c := range bp.StatusCheckContexts {
		if c == "gitea-mq" {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func (f *giteaForge) GetCheckStates(ctx context.Context, owner, name, sha string) (map[string]forge.CheckState, error) {
	cs, err := f.client.GetCombinedCommitStatus(ctx, owner, name, sha)
	if err != nil {
		return nil, err
	}
	// Exclude our own status so the monitor never gates on it.
	out := make(map[string]forge.CheckState, len(cs.Statuses))
	for _, s := range cs.Statuses {
		if s.Context == "gitea-mq" {
			continue
		}
		out[s.Context] = MapState(s.Status)
	}
	return out, nil
}

func (f *giteaForge) CreateMergeBranch(ctx context.Context, owner, name, base, headSHA, branch string) (string, bool, error) {
	res, err := f.client.MergeBranches(ctx, owner, name, base, headSHA, branch)
	if err != nil {
		if IsMergeConflict(err) {
			return "", true, nil
		}
		return "", false, err
	}
	return res.SHA, false, nil
}

func (f *giteaForge) DeleteBranch(ctx context.Context, owner, name, branch string) error {
	return f.client.DeleteBranch(ctx, owner, name, branch)
}

func (f *giteaForge) ListBranches(ctx context.Context, owner, name string) ([]string, error) {
	bs, err := f.client.ListBranches(ctx, owner, name)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.Name
	}
	return out, nil
}

func (f *giteaForge) CancelAutoMerge(ctx context.Context, owner, name string, number int64) error {
	return f.client.CancelAutoMerge(ctx, owner, name, number)
}

func (f *giteaForge) Comment(ctx context.Context, owner, name string, number int64, body string) error {
	return f.client.CreateComment(ctx, owner, name, number, body)
}

func (f *giteaForge) EnsureRepoSetup(ctx context.Context, owner, name string, cfg forge.SetupConfig) error {
	if err := EnsureBranchProtection(ctx, f.client, owner, name); err != nil {
		return err
	}
	if cfg.ExternalURL == "" {
		// No public URL → Gitea has nowhere to deliver webhooks; the
		// reconcile poll covers us.
		return nil
	}
	webhookURL := strings.TrimRight(cfg.ExternalURL, "/") + "/webhook/gitea"
	return EnsureWebhook(ctx, f.client, owner, name, webhookURL, cfg.WebhookSecret)
}
