package github

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"

	gh "github.com/google/go-github/v84/github"

	"github.com/Mic92/gitea-mq/internal/forge"
)

const labelColor = "0e8a16"

var _ forge.Labeler = (*githubForge)(nil)

// Created explicitly because GitHub's auto-created labels are plain grey.
type ensuredLabels struct {
	mu   sync.Mutex
	done map[string]bool
}

func (f *githubForge) ensured(owner, name string) *ensuredLabels {
	c, _ := f.labels.LoadOrStore(strings.ToLower(owner+"/"+name), &ensuredLabels{done: map[string]bool{}})
	return c.(*ensuredLabels)
}

// Rate limits also answer 403 but are transient.
func mapLabelErr(err error) error {
	var rl *gh.RateLimitError
	var abuse *gh.AbuseRateLimitError
	if errors.As(err, &rl) || errors.As(err, &abuse) {
		return err
	}
	var ghErr *gh.ErrorResponse
	if errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusForbidden {
		return errors.Join(forge.ErrLabelsForbidden, err)
	}
	return err
}

func (f *githubForge) ApplyLabels(ctx context.Context, owner, name string, number int64, add, remove []string) error {
	c, err := f.app.ClientForRepo(owner, name)
	if err != nil {
		return err
	}
	if len(add) > 0 {
		e := f.ensured(owner, name)
		e.mu.Lock()
		defer e.mu.Unlock()
		for _, l := range add {
			if e.done[l] {
				continue
			}
			_, resp, err := c.Issues.CreateLabel(ctx, owner, name, &gh.Label{Name: gh.Ptr(l), Color: gh.Ptr(labelColor)})
			if err != nil && (resp == nil || resp.StatusCode != http.StatusUnprocessableEntity) {
				return mapLabelErr(err)
			}
			e.done[l] = true
		}
		if _, _, err := c.Issues.AddLabelsToIssue(ctx, owner, name, int(number), add); err != nil {
			return mapLabelErr(err)
		}
	}
	for _, l := range remove {
		if err := f.RemoveLabel(ctx, owner, name, number, l); err != nil {
			return mapLabelErr(err)
		}
	}
	return nil
}

func (f *githubForge) DeleteLabel(ctx context.Context, owner, name, label string) error {
	c, err := f.app.ClientForRepo(owner, name)
	if err != nil {
		return err
	}
	e := f.ensured(owner, name)
	e.mu.Lock()
	defer e.mu.Unlock()
	resp, err := c.Issues.DeleteLabel(ctx, owner, name, url.PathEscape(label))
	delete(e.done, label)
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return mapLabelErr(err)
}
