// Package web provides the server-rendered HTML dashboard for gitea-mq.
// No JavaScript frameworks — pages are functional with JS disabled, using
// <meta http-equiv="refresh"> for auto-refresh.
package web

import (
	"embed"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/jogman/gitea-mq/internal/config"
	"github.com/jogman/gitea-mq/internal/gitea"
	"github.com/jogman/gitea-mq/internal/queue"
	"github.com/jogman/gitea-mq/internal/store/pg"
)

//go:embed templates/*.html
var templateFS embed.FS

// funcMap provides template helper functions.
var funcMap = template.FuncMap{
	"inc": func(i int) int { return i + 1 },
	"checkIcon": func(state pg.CheckState) string {
		switch state {
		case pg.CheckStateSuccess:
			return "✅"
		case pg.CheckStateFailure, pg.CheckStateError:
			return "❌"
		default:
			return "⏳"
		}
	},
}

var templates = template.Must(
	template.New("").Funcs(funcMap).ParseFS(templateFS, "templates/*.html"),
)

// RepoOverview holds the data for one repo in the overview page.
type RepoOverview struct {
	Owner     string
	Name      string
	QueueSize int
}

// OverviewData is the template data for the overview page.
type OverviewData struct {
	Repos           []RepoOverview
	RefreshInterval int // seconds
}

// RepoDetailEntry holds one queue entry for the repo detail page.
type RepoDetailEntry struct {
	PrNumber     int64
	TargetBranch string
	State        string
}

// RepoDetailData is the template data for the repo detail page.
type RepoDetailData struct {
	Owner           string
	Name            string
	Entries         []RepoDetailEntry
	RefreshInterval int // seconds
}

// PRDetailData is the template data for the PR detail page.
type PRDetailData struct {
	Owner           string
	Name            string
	PrNumber        int64
	Title           string
	Author          string
	State           string
	Position        int
	EnqueuedAt      string
	CheckStatuses   []pg.CheckStatus
	InQueue         bool
	RefreshInterval int // seconds
}

// RepoLister abstracts how the dashboard gets the current managed repo set.
// Implementations include the RepoRegistry (dynamic) and static lists (tests).
type RepoLister interface {
	List() []config.RepoRef
	Contains(fullName string) bool
}

// Deps holds the dependencies the web handlers need.
type Deps struct {
	Queue           *queue.Service
	Repos           RepoLister
	Gitea           gitea.Client
	RefreshInterval int // seconds
}

// NewMux creates an http.ServeMux with the dashboard routes registered.
func NewMux(deps *Deps) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/static/style.css", staticCSSHandler)
	mux.HandleFunc("/", overviewHandler(deps))
	mux.HandleFunc("/repo/", repoHandler(deps))
	return mux
}

// staticCSSHandler serves the shared stylesheet from the embedded FS.
func staticCSSHandler(w http.ResponseWriter, _ *http.Request) {
	data, err := templateFS.ReadFile("templates/style.css")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(data)
}

// overviewHandler serves the overview page at GET /.
func overviewHandler(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Only match exact root path for the overview.
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		ctx := r.Context()
		data := OverviewData{
			RefreshInterval: deps.RefreshInterval,
		}

		for _, ref := range deps.Repos.List() {
			overview := RepoOverview{Owner: ref.Owner, Name: ref.Name}

			repo, err := deps.Queue.GetOrCreateRepo(ctx, ref.Owner, ref.Name)
			if err != nil {
				slog.Error("failed to get repo", "repo", ref, "error", err)
				data.Repos = append(data.Repos, overview)
				continue
			}

			entries, err := deps.Queue.ListActiveEntries(ctx, repo.ID)
			if err != nil {
				slog.Error("failed to list active entries", "repo", ref, "error", err)
				data.Repos = append(data.Repos, overview)
				continue
			}

			overview.QueueSize = len(entries)
			data.Repos = append(data.Repos, overview)
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.ExecuteTemplate(w, "overview.html", data); err != nil {
			slog.Error("failed to render overview", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
	}
}

// repoHandler serves repo and PR detail pages:
//   - GET /repo/{owner}/{name} — repo queue listing
//   - GET /repo/{owner}/{name}/pr/{number} — PR detail
func repoHandler(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Parse /repo/{owner}/{name}[/pr/{number}] from path.
		path := strings.TrimPrefix(r.URL.Path, "/repo/")
		owner, rest, ok := strings.Cut(path, "/")
		if !ok || owner == "" || rest == "" {
			http.NotFound(w, r)
			return
		}

		// Split rest into name and optional /pr/{number}.
		var name string
		var prNumberStr string
		if idx := strings.Index(rest, "/"); idx >= 0 {
			name = rest[:idx]
			suffix := rest[idx+1:] // e.g. "pr/42"
			prPrefix, numStr, hasPR := strings.Cut(suffix, "/")
			if !hasPR || prPrefix != "pr" || numStr == "" {
				http.NotFound(w, r)
				return
			}
			prNumberStr = numStr
		} else {
			name = rest
		}

		if name == "" {
			http.NotFound(w, r)
			return
		}

		// Check if this is a managed repo.
		if !deps.Repos.Contains(owner + "/" + name) {
			http.NotFound(w, r)
			return
		}

		if prNumberStr != "" {
			servePRDetail(w, r, deps, owner, name, prNumberStr)
		} else {
			serveRepoDetail(w, r, deps, owner, name)
		}
	}
}

// serveRepoDetail renders the repo queue listing page.
func serveRepoDetail(w http.ResponseWriter, r *http.Request, deps *Deps, owner, name string) {
	ctx := r.Context()
	repo, err := deps.Queue.GetOrCreateRepo(ctx, owner, name)
	if err != nil {
		slog.Error("failed to get repo", "owner", owner, "name", name, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	entries, err := deps.Queue.ListActiveEntries(ctx, repo.ID)
	if err != nil {
		slog.Error("failed to list active entries", "owner", owner, "name", name, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	data := RepoDetailData{
		Owner:           owner,
		Name:            name,
		RefreshInterval: deps.RefreshInterval,
	}

	for _, e := range entries {
		data.Entries = append(data.Entries, RepoDetailEntry{
			PrNumber:     e.PrNumber,
			TargetBranch: e.TargetBranch,
			State:        string(e.State),
		})
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "repo.html", data); err != nil {
		slog.Error("failed to render repo detail", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

// servePRDetail renders the PR detail page.
func servePRDetail(w http.ResponseWriter, r *http.Request, deps *Deps, owner, name, prNumberStr string) {
	prNumber, err := strconv.ParseInt(prNumberStr, 10, 64)
	if err != nil || prNumber <= 0 {
		http.NotFound(w, r)
		return
	}

	ctx := r.Context()
	repo, err := deps.Queue.GetOrCreateRepo(ctx, owner, name)
	if err != nil {
		slog.Error("failed to get repo", "owner", owner, "name", name, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	entry, err := deps.Queue.GetEntry(ctx, repo.ID, prNumber)
	if err != nil {
		slog.Error("failed to get entry", "pr", prNumber, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	data := PRDetailData{
		Owner:           owner,
		Name:            name,
		PrNumber:        prNumber,
		Title:           "—",
		Author:          "—",
		RefreshInterval: deps.RefreshInterval,
	}

	if entry == nil {
		// PR not in queue — render friendly page.
		data.InQueue = false
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.ExecuteTemplate(w, "pr.html", data); err != nil {
			slog.Error("failed to render PR detail", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
		return
	}

	data.InQueue = true
	data.State = string(entry.State)
	if entry.EnqueuedAt.Valid {
		data.EnqueuedAt = entry.EnqueuedAt.Time.Format("2006-01-02 15:04:05 UTC")
	}

	// Determine queue position.
	entries, err := deps.Queue.ListActiveEntries(ctx, repo.ID)
	if err != nil {
		slog.Error("failed to list active entries", "error", err)
	} else {
		for i, e := range entries {
			if e.PrNumber == prNumber {
				data.Position = i + 1
				break
			}
		}
	}

	// Fetch PR title/author from Gitea API (graceful degradation).
	if deps.Gitea != nil {
		pr, err := deps.Gitea.GetPR(ctx, owner, name, prNumber)
		if err != nil {
			slog.Warn("failed to fetch PR from Gitea", "pr", prNumber, "error", err)
		} else {
			data.Title = pr.Title
			if pr.User != nil {
				data.Author = pr.User.Login
			}
		}
	}

	// Fetch check statuses if head-of-queue in testing state.
	if data.Position == 1 && entry.State == pg.EntryStateTesting {
		checks, err := deps.Queue.GetCheckStatuses(ctx, entry.ID)
		if err != nil {
			slog.Error("failed to get check statuses", "pr", prNumber, "error", err)
		} else {
			data.CheckStatuses = checks
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "pr.html", data); err != nil {
		slog.Error("failed to render PR detail", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}
