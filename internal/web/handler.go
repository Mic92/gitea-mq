// Package web provides the server-rendered HTML dashboard for gitea-mq.
// No JavaScript frameworks — pages are functional with JS disabled, using
// <meta http-equiv="refresh"> for auto-refresh.
package web

import (
	"embed"
	"html/template"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jogman/gitea-mq/internal/config"
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
	Head      *HeadEntry
}

// HeadEntry is a simplified view of the head-of-queue entry.
type HeadEntry struct {
	PrNumber int64
	State    string
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
	HeadPR          int64
	CheckStatuses   []pg.CheckStatus
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
			if len(entries) > 0 {
				overview.Head = &HeadEntry{
					PrNumber: entries[0].PrNumber,
					State:    string(entries[0].State),
				}
			}

			data.Repos = append(data.Repos, overview)
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.ExecuteTemplate(w, "overview.html", data); err != nil {
			slog.Error("failed to render overview", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
	}
}

// repoHandler serves the repo detail page at GET /repo/{owner}/{name}.
func repoHandler(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Parse /repo/{owner}/{name} from path.
		path := strings.TrimPrefix(r.URL.Path, "/repo/")
		owner, name, ok := strings.Cut(path, "/")
		if !ok || owner == "" || name == "" {
			http.NotFound(w, r)
			return
		}

		// Strip any trailing path segments.
		if idx := strings.Index(name, "/"); idx >= 0 {
			name = name[:idx]
		}

		// Check if this is a managed repo.
		if !deps.Repos.Contains(owner + "/" + name) {
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

		// Get check statuses for head-of-queue.
		if len(entries) > 0 {
			head := entries[0]
			data.HeadPR = head.PrNumber

			if head.State == pg.EntryStateTesting {
				checks, err := deps.Queue.GetCheckStatuses(ctx, head.ID)
				if err != nil {
					slog.Error("failed to get check statuses", "pr", head.PrNumber, "error", err)
				} else {
					data.CheckStatuses = checks
				}
			}
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.ExecuteTemplate(w, "repo.html", data); err != nil {
			slog.Error("failed to render repo detail", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
	}
}
