// Package github implements forge.Forge for GitHub via a GitHub App.
package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/bradleyfalzon/ghinstallation/v2"
	gh "github.com/google/go-github/v84/github"

	"github.com/Mic92/gitea-mq/internal/forge"
)

// DefaultBaseURL is github.com's REST root. Tests inject a ghfake URL.
const DefaultBaseURL = "https://api.github.com"

// App holds a GitHub App identity and vends per-installation API clients.
//
// GitHub Apps cannot act on a repository without an installation that covers
// it, so the repo→installation map is the routing table for every API call.
type App struct {
	appID   int64
	baseURL string // REST root, no trailing slash
	atr     *ghinstallation.AppsTransport

	// appClient is JWT-authenticated and may only call /app/* endpoints.
	appClient *gh.Client

	mu          sync.Mutex
	instClients map[int64]*gh.Client
	repoInstall map[string]int64 // owner/name -> installation ID
}

// NewApp constructs an App. baseURL must be the REST root (e.g.
// https://api.github.com or http://127.0.0.1:PORT/api/v3 for ghfake).
func NewApp(appID int64, privateKey []byte, baseURL string) (*App, error) {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	atr, err := ghinstallation.NewAppsTransport(http.DefaultTransport, appID, privateKey)
	if err != nil {
		return nil, fmt.Errorf("github app transport: %w", err)
	}
	atr.BaseURL = baseURL

	appClient, err := newClient(&http.Client{Transport: atr}, baseURL)
	if err != nil {
		return nil, err
	}
	return &App{
		appID:       appID,
		baseURL:     baseURL,
		atr:         atr,
		appClient:   appClient,
		instClients: map[int64]*gh.Client{},
		repoInstall: map[string]int64{},
	}, nil
}

// newClient builds a go-github client rooted at baseURL. For github.com the
// enterprise-URL helper would mangle the path, so it is special-cased.
func newClient(hc *http.Client, baseURL string) (*gh.Client, error) {
	if baseURL == DefaultBaseURL {
		return gh.NewClient(hc), nil
	}
	return gh.NewClient(hc).WithEnterpriseURLs(baseURL, baseURL)
}

func (a *App) AppID() int64 { return a.appID }

// graphqlURL derives the GraphQL endpoint from the REST base. github.com and
// GHES place it differently relative to the REST root.
func (a *App) graphqlURL() string {
	if a.baseURL == DefaultBaseURL {
		return "https://api.github.com/graphql"
	}
	return strings.TrimSuffix(a.baseURL, "/api/v3") + "/api/graphql"
}

func (a *App) installationClient(id int64) (*gh.Client, error) {
	a.mu.Lock()
	if c, ok := a.instClients[id]; ok {
		a.mu.Unlock()
		return c, nil
	}
	a.mu.Unlock()

	itr := ghinstallation.NewFromAppsTransport(a.atr, id)
	itr.BaseURL = a.baseURL
	c, err := newClient(&http.Client{Transport: itr}, a.baseURL)
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	a.instClients[id] = c
	a.mu.Unlock()
	return c, nil
}

// Refresh repopulates the repo→installation map from the App's current
// installations. Called at startup and on installation* webhooks so newly
// installed repos become routable without restart.
func (a *App) Refresh(ctx context.Context) error {
	repoInstall := map[string]int64{}

	for inst, err := range a.appClient.Apps.ListInstallationsIter(ctx, &gh.ListOptions{PerPage: 100}) {
		if err != nil {
			return fmt.Errorf("list installations: %w", err)
		}
		ic, err := a.installationClient(inst.GetID())
		if err != nil {
			return err
		}
		for repo, err := range ic.Apps.ListReposIter(ctx, &gh.ListOptions{PerPage: 100}) {
			if err != nil {
				return fmt.Errorf("list repos for installation %d: %w", inst.GetID(), err)
			}
			repoInstall[repo.GetFullName()] = inst.GetID()
		}
	}

	a.mu.Lock()
	a.repoInstall = repoInstall
	a.mu.Unlock()
	return nil
}

func (a *App) Repos() []forge.RepoRef {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]forge.RepoRef, 0, len(a.repoInstall))
	for full := range a.repoInstall {
		owner, name, ok := strings.Cut(full, "/")
		if !ok {
			continue
		}
		out = append(out, forge.RepoRef{Forge: forge.KindGithub, Owner: owner, Name: name})
	}
	return out
}

func (a *App) ClientForRepo(owner, name string) (*gh.Client, error) {
	a.mu.Lock()
	id, ok := a.repoInstall[owner+"/"+name]
	a.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("github: no installation covers %s/%s", owner, name)
	}
	return a.installationClient(id)
}
