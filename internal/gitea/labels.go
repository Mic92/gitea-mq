package gitea

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/Mic92/gitea-mq/internal/forge"
)

const labelColor = "#0e8a16"

var _ forge.Labeler = (*giteaForge)(nil)

// repoLabels caches labels by lowercase name; Gitea's issue endpoints take ids.
type repoLabels struct {
	mu     sync.Mutex
	loaded bool
	byName map[string]Label
}

func (f *giteaForge) labelCache(owner, name string) *repoLabels {
	key := strings.ToLower(owner + "/" + name)
	c, _ := f.labels.LoadOrStore(key, &repoLabels{})
	return c.(*repoLabels)
}

func (c *repoLabels) load(ctx context.Context, f *giteaForge, owner, name string, force bool) error {
	if c.loaded && !force {
		return nil
	}
	ls, err := f.client.ListRepoLabels(ctx, owner, name)
	if err != nil {
		return err
	}
	c.byName = make(map[string]Label, len(ls))
	for _, l := range ls {
		c.byName[strings.ToLower(l.Name)] = l
	}
	c.loaded = true
	return nil
}

func scopeOf(name string) string {
	i := strings.LastIndex(name, "/")
	if i <= 0 || i == len(name)-1 {
		return ""
	}
	return name[:i]
}

func mapLabelErr(err error) error {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusForbidden {
		return errors.Join(forge.ErrLabelsForbidden, err)
	}
	return err
}

// Scoped names are created exclusive so one add replaces the previous state.
func (c *repoLabels) ensure(ctx context.Context, f *giteaForge, owner, name, label string) (Label, error) {
	if l, ok := c.byName[strings.ToLower(label)]; ok {
		return l, nil
	}
	l, err := f.client.CreateRepoLabel(ctx, owner, name, label, labelColor, scopeOf(label) != "")
	if err != nil {
		return Label{}, err
	}
	l.Name = label
	c.byName[strings.ToLower(label)] = *l
	return *l, nil
}

func (f *giteaForge) ApplyLabels(ctx context.Context, owner, name string, number int64, add, remove []string) error {
	c := f.labelCache(owner, name)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.load(ctx, f, owner, name, false); err != nil {
		return mapLabelErr(err)
	}

	if len(add) > 0 {
		if err := f.addLabels(ctx, c, owner, name, number, add); err != nil {
			return mapLabelErr(err)
		}
	}

	for _, r := range remove {
		rl, ok := c.byName[strings.ToLower(r)]
		if !ok {
			continue
		}
		if swappedByScope(c, rl, add) {
			continue
		}
		if err := f.client.RemoveIssueLabel(ctx, owner, name, number, rl.ID); err != nil {
			return mapLabelErr(err)
		}
	}
	return nil
}

// swappedByScope: Gitea already dropped rl when an exclusive label of its scope was added.
func swappedByScope(c *repoLabels, rl Label, added []string) bool {
	scope := scopeOf(rl.Name)
	if !rl.Exclusive || scope == "" {
		return false
	}
	for _, a := range added {
		if al, ok := c.byName[strings.ToLower(a)]; ok && al.Exclusive && scopeOf(al.Name) == scope {
			return true
		}
	}
	return false
}

// Gitea silently ignores unknown ids, so a missing label in the result means a stale cache.
func (f *giteaForge) addLabels(ctx context.Context, c *repoLabels, owner, name string, number int64, labels []string) error {
	for attempt := range 2 {
		ids := make([]int64, 0, len(labels))
		for _, l := range labels {
			lbl, err := c.ensure(ctx, f, owner, name, l)
			if err != nil {
				return err
			}
			ids = append(ids, lbl.ID)
		}
		got, err := f.client.AddIssueLabels(ctx, owner, name, number, ids)
		if err != nil {
			return err
		}
		if hasAll(got, labels) {
			return nil
		}
		if attempt == 0 {
			if err := c.load(ctx, f, owner, name, true); err != nil {
				return err
			}
		}
	}
	return nil
}

func hasAll(have []Label, want []string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			if strings.EqualFold(h.Name, w) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (f *giteaForge) DeleteLabel(ctx context.Context, owner, name, label string) error {
	c := f.labelCache(owner, name)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.load(ctx, f, owner, name, false); err != nil {
		return mapLabelErr(err)
	}
	l, ok := c.byName[strings.ToLower(label)]
	if !ok {
		return nil
	}
	if err := f.client.DeleteRepoLabel(ctx, owner, name, l.ID); err != nil {
		return mapLabelErr(err)
	}
	delete(c.byName, strings.ToLower(label))
	return nil
}
