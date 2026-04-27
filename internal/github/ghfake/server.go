// It is not a faithful simulator: only the endpoints gitea-mq calls are
// implemented, with just enough state to drive the adapter and integration
// tests deterministically without network access.
package ghfake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gh "github.com/google/go-github/v84/github"
)

type Installation struct {
	ID    int64
	Repos []string // owner/name
}

type PR struct {
	Number    int64
	Title     string
	State     string // open, closed
	Merged    bool
	User      string
	HeadRef   string
	HeadSHA   string
	BaseRef   string
	NodeID    string
	AutoMerge bool
	HTMLURL   string
}

type CheckRun struct {
	ID         int64
	Name       string
	HeadSHA    string
	Status     string
	Conclusion string
	DetailsURL string
	Output     struct{ Title, Summary string }
}

type Ruleset struct {
	ID          int64           `json:"id"`
	Name        string          `json:"name"`
	Target      string          `json:"target"`
	Enforcement string          `json:"enforcement"`
	Conditions  json.RawMessage `json:"conditions,omitempty"`
	Rules       []RulesetRule   `json:"rules"`
}

type RulesetRule struct {
	Type       string          `json:"type"`
	Parameters json.RawMessage `json:"parameters,omitempty"`
}

type Repo struct {
	Owner, Name   string
	DefaultBranch string

	PRs       map[int64]*PR
	Refs      map[string]string // branch -> sha
	CheckRuns map[string][]*CheckRun
	Rulesets  []*Ruleset
	// ConflictOn[head] makes POST /merges with that head return 409.
	ConflictOn map[string]bool
	// Settings tracks PATCH /repos/{o}/{r} keys.
	Settings map[string]any

	// RequiredChecks[branch] feeds /rules/branches/{b} as a synthetic
	// required_status_checks rule, decoupled from Rulesets so tests can
	// stub rule evaluation directly.
	RequiredChecks map[string][]string
}

type Server struct {
	*httptest.Server

	mu       sync.Mutex
	installs map[int64]*Installation
	repos    map[string]*Repo // owner/name
	idSeq    atomic.Int64
}

func New() *Server {
	s := &Server{
		installs: map[int64]*Installation{},
		repos:    map[string]*Repo{},
	}
	mux := http.NewServeMux()
	s.routes(mux)
	s.Server = httptest.NewServer(mux)
	return s
}

func (s *Server) nextID() int64 { return s.idSeq.Add(1) }

func (s *Server) AddInstallation(id int64, fullNames ...string) *Installation {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := &Installation{ID: id, Repos: append([]string(nil), fullNames...)}
	s.installs[id] = inst
	return inst
}

func (s *Server) AddRepo(owner, name string) *Repo {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := &Repo{
		Owner:          owner,
		Name:           name,
		DefaultBranch:  "main",
		PRs:            map[int64]*PR{},
		Refs:           map[string]string{"main": "sha-main"},
		CheckRuns:      map[string][]*CheckRun{},
		ConflictOn:     map[string]bool{},
		Settings:       map[string]any{},
		RequiredChecks: map[string][]string{},
	}
	s.repos[owner+"/"+name] = r
	return r
}

func (s *Server) AddPR(owner, name string, pr PR) *PR {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.repos[owner+"/"+name]
	if pr.State == "" {
		pr.State = "open"
	}
	if pr.NodeID == "" {
		pr.NodeID = fmt.Sprintf("PR_%s_%d", name, pr.Number)
	}
	if pr.HTMLURL == "" {
		pr.HTMLURL = fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, name, pr.Number)
	}
	cp := pr
	r.PRs[pr.Number] = &cp
	return r.PRs[pr.Number]
}

func (s *Server) Repo(owner, name string) *Repo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.repos[owner+"/"+name]
}

func (s *Server) Client() *gh.Client {
	c, err := gh.NewClient(nil).WithEnterpriseURLs(s.URL, s.URL)
	if err != nil {
		panic(err)
	}
	return c
}

// --- HTTP ---

const apiV3 = "/api/v3"

func (s *Server) routes(mux *http.ServeMux) {
	// App / installation.
	mux.HandleFunc("GET "+apiV3+"/app/installations", s.hListInstalls)
	mux.HandleFunc("POST "+apiV3+"/app/installations/{id}/access_tokens", s.hAccessToken)
	mux.HandleFunc("GET "+apiV3+"/installation/repositories", s.hInstallRepos)

	// Repos.
	mux.HandleFunc("GET "+apiV3+"/repos/{o}/{r}", s.hGetRepo)
	mux.HandleFunc("PATCH "+apiV3+"/repos/{o}/{r}", s.hPatchRepo)

	// Pulls.
	mux.HandleFunc("GET "+apiV3+"/repos/{o}/{r}/pulls", s.hListPRs)
	mux.HandleFunc("POST "+apiV3+"/repos/{o}/{r}/pulls", s.hCreatePR)
	mux.HandleFunc("GET "+apiV3+"/repos/{o}/{r}/pulls/{n}", s.hGetPR)

	// Issues (comments).
	mux.HandleFunc("POST "+apiV3+"/repos/{o}/{r}/issues/{n}/comments", s.hCreateComment)

	// Check runs.
	mux.HandleFunc("POST "+apiV3+"/repos/{o}/{r}/check-runs", s.hCreateCheckRun)
	mux.HandleFunc("PATCH "+apiV3+"/repos/{o}/{r}/check-runs/{id}", s.hUpdateCheckRun)
	mux.HandleFunc("GET "+apiV3+"/repos/{o}/{r}/commits/{sha}/check-runs", s.hListCheckRuns)
	mux.HandleFunc("GET "+apiV3+"/repos/{o}/{r}/commits/{sha}/statuses", s.hListStatuses)

	// Git refs / branches.
	mux.HandleFunc("POST "+apiV3+"/repos/{o}/{r}/git/refs", s.hCreateRef)
	mux.HandleFunc("DELETE "+apiV3+"/repos/{o}/{r}/git/refs/{ref...}", s.hDeleteRef)
	mux.HandleFunc("GET "+apiV3+"/repos/{o}/{r}/branches", s.hListBranches)
	mux.HandleFunc("POST "+apiV3+"/repos/{o}/{r}/merges", s.hMerge)

	// Rules / rulesets.
	mux.HandleFunc("GET "+apiV3+"/repos/{o}/{r}/rules/branches/{b}", s.hRulesForBranch)
	mux.HandleFunc("GET "+apiV3+"/repos/{o}/{r}/rulesets", s.hListRulesets)
	mux.HandleFunc("POST "+apiV3+"/repos/{o}/{r}/rulesets", s.hCreateRuleset)

	// GraphQL.
	mux.HandleFunc("POST /api/graphql", s.hGraphQL)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "ghfake: unhandled "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	})
}

func (s *Server) repo(r *http.Request) (*Repo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	repo, ok := s.repos[r.PathValue("o")+"/"+r.PathValue("r")]
	return repo, ok
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// --- handlers: app ---

func (s *Server) hListInstalls(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	out := make([]map[string]any, 0, len(s.installs))
	for _, in := range s.installs {
		out = append(out, map[string]any{"id": in.ID, "app_id": 1})
	}
	s.mu.Unlock()
	writeJSON(w, 200, out)
}

func (s *Server) hAccessToken(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 201, map[string]any{
		"token":      "ghs_fake_" + r.PathValue("id"),
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
}

func (s *Server) hInstallRepos(w http.ResponseWriter, r *http.Request) {
	// The fake routes by token: ghs_fake_<installID>. This avoids replicating
	// JWT parsing while still letting tests verify per-installation scoping.
	auth := r.Header.Get("Authorization")
	var instID int64
	if i := strings.LastIndex(auth, "_"); i >= 0 {
		instID, _ = strconv.ParseInt(auth[i+1:], 10, 64)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	inst := s.installs[instID]
	repos := make([]map[string]any, 0)
	if inst != nil {
		for _, full := range inst.Repos {
			if rp := s.repos[full]; rp != nil {
				repos = append(repos, repoJSON(rp))
			}
		}
	}
	writeJSON(w, 200, map[string]any{"total_count": len(repos), "repositories": repos})
}

// --- handlers: repo ---

func repoJSON(r *Repo) map[string]any {
	return map[string]any{
		"id":             1,
		"name":           r.Name,
		"full_name":      r.Owner + "/" + r.Name,
		"owner":          map[string]any{"login": r.Owner},
		"default_branch": r.DefaultBranch,
		"html_url":       "https://github.com/" + r.Owner + "/" + r.Name,
	}
}

func (s *Server) hGetRepo(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.repo(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, 200, repoJSON(rp))
}

func (s *Server) hPatchRepo(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.repo(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	for k, v := range body {
		rp.Settings[k] = v
	}
	s.mu.Unlock()
	writeJSON(w, 200, repoJSON(rp))
}

// --- handlers: pulls ---

func prJSON(owner, name string, p *PR) map[string]any {
	var am any
	if p.AutoMerge {
		am = map[string]any{"enabled_by": map[string]any{"login": p.User}}
	}
	return map[string]any{
		"number":   p.Number,
		"node_id":  p.NodeID,
		"title":    p.Title,
		"state":    p.State,
		"merged":   p.Merged,
		"html_url": p.HTMLURL,
		"user":     map[string]any{"login": p.User},
		"head": map[string]any{
			"ref": p.HeadRef, "sha": p.HeadSHA,
			"repo": map[string]any{"full_name": owner + "/" + name},
		},
		"base":       map[string]any{"ref": p.BaseRef},
		"auto_merge": am,
	}
}

func (s *Server) hListPRs(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.repo(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	state := r.URL.Query().Get("state")
	s.mu.Lock()
	out := make([]map[string]any, 0, len(rp.PRs))
	for _, p := range rp.PRs {
		if state != "" && state != "all" && p.State != state {
			continue
		}
		out = append(out, prJSON(rp.Owner, rp.Name, p))
	}
	s.mu.Unlock()
	writeJSON(w, 200, out)
}

func (s *Server) hGetPR(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.repo(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	n, _ := strconv.ParseInt(r.PathValue("n"), 10, 64)
	s.mu.Lock()
	p := rp.PRs[n]
	s.mu.Unlock()
	if p == nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, 200, prJSON(rp.Owner, rp.Name, p))
}

func (s *Server) hCreatePR(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.repo(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var body struct{ Title, Head, Base string }
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	n := int64(len(rp.PRs) + 1)
	p := &PR{
		Number: n, Title: body.Title, HeadRef: body.Head, BaseRef: body.Base, State: "open",
		HeadSHA: rp.Refs[body.Head], NodeID: fmt.Sprintf("PR_%s_%d", rp.Name, n),
	}
	rp.PRs[n] = p
	s.mu.Unlock()
	writeJSON(w, 201, prJSON(rp.Owner, rp.Name, p))
}

func (s *Server) hCreateComment(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.repo(r); !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, 201, map[string]any{"id": s.nextID()})
}

// --- handlers: check runs ---

func checkRunJSON(c *CheckRun) map[string]any {
	out := map[string]any{
		"id": c.ID, "name": c.Name, "head_sha": c.HeadSHA,
		"status": c.Status, "details_url": c.DetailsURL,
		"output": map[string]any{"title": c.Output.Title, "summary": c.Output.Summary},
	}
	if c.Conclusion != "" {
		out["conclusion"] = c.Conclusion
	}
	return out
}

func (s *Server) hCreateCheckRun(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.repo(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var body struct {
		Name       string `json:"name"`
		HeadSHA    string `json:"head_sha"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		DetailsURL string `json:"details_url"`
		Output     struct{ Title, Summary string }
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	cr := &CheckRun{
		ID: s.nextID(), Name: body.Name, HeadSHA: body.HeadSHA,
		Status: body.Status, Conclusion: body.Conclusion, DetailsURL: body.DetailsURL,
		Output: body.Output,
	}
	s.mu.Lock()
	rp.CheckRuns[body.HeadSHA] = append(rp.CheckRuns[body.HeadSHA], cr)
	s.mu.Unlock()
	writeJSON(w, 201, checkRunJSON(cr))
}

func (s *Server) hUpdateCheckRun(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.repo(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		DetailsURL string `json:"details_url"`
		Output     struct{ Title, Summary string }
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, runs := range rp.CheckRuns {
		for _, cr := range runs {
			if cr.ID != id {
				continue
			}
			if body.Status != "" {
				cr.Status = body.Status
			}
			if body.Conclusion != "" {
				cr.Conclusion = body.Conclusion
			}
			if body.DetailsURL != "" {
				cr.DetailsURL = body.DetailsURL
			}
			if body.Output.Title != "" || body.Output.Summary != "" {
				cr.Output = body.Output
			}
			writeJSON(w, 200, checkRunJSON(cr))
			return
		}
	}
	http.NotFound(w, r)
}

func (s *Server) hListCheckRuns(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.repo(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	sha := r.PathValue("sha")
	nameFilter := r.URL.Query().Get("check_name")
	s.mu.Lock()
	var out []map[string]any
	for _, cr := range rp.CheckRuns[sha] {
		if nameFilter != "" && cr.Name != nameFilter {
			continue
		}
		out = append(out, checkRunJSON(cr))
	}
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"total_count": len(out), "check_runs": out})
}

func (s *Server) hListStatuses(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.repo(r); !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, 200, []any{})
}

// --- handlers: refs / merge ---

func (s *Server) hCreateRef(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.repo(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var body struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	branch := strings.TrimPrefix(body.Ref, "refs/heads/")
	s.mu.Lock()
	if _, exists := rp.Refs[branch]; exists {
		s.mu.Unlock()
		writeJSON(w, 422, map[string]any{"message": "Reference already exists"})
		return
	}
	rp.Refs[branch] = body.SHA
	s.mu.Unlock()
	writeJSON(w, 201, map[string]any{"ref": body.Ref, "object": map[string]any{"sha": body.SHA}})
}

func (s *Server) hDeleteRef(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.repo(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	branch := strings.TrimPrefix(r.PathValue("ref"), "heads/")
	s.mu.Lock()
	delete(rp.Refs, branch)
	s.mu.Unlock()
	w.WriteHeader(204)
}

func (s *Server) hListBranches(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.repo(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	out := make([]map[string]any, 0, len(rp.Refs))
	for b, sha := range rp.Refs {
		out = append(out, map[string]any{"name": b, "commit": map[string]any{"sha": sha}})
	}
	s.mu.Unlock()
	writeJSON(w, 200, out)
}

func (s *Server) hMerge(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.repo(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var body struct{ Base, Head, CommitMessage string }
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	defer s.mu.Unlock()
	if rp.ConflictOn[body.Head] {
		writeJSON(w, 409, map[string]any{"message": "Merge conflict"})
		return
	}
	if _, ok := rp.Refs[body.Base]; !ok {
		http.NotFound(w, r)
		return
	}
	mergeSHA := fmt.Sprintf("merge-%s-%s", body.Base, body.Head)
	rp.Refs[body.Base] = mergeSHA
	writeJSON(w, 201, map[string]any{"sha": mergeSHA})
}

// --- handlers: rules ---

func (s *Server) hRulesForBranch(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.repo(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	b := r.PathValue("b")
	s.mu.Lock()
	checks := rp.RequiredChecks[b]
	s.mu.Unlock()
	if len(checks) == 0 {
		writeJSON(w, 200, []any{})
		return
	}
	var rc []map[string]any
	for _, c := range checks {
		rc = append(rc, map[string]any{"context": c})
	}
	writeJSON(w, 200, []any{
		map[string]any{
			"type": "required_status_checks",
			"parameters": map[string]any{
				"required_status_checks": rc,
			},
		},
	})
}

func (s *Server) hListRulesets(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.repo(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, 200, rp.Rulesets)
}

func (s *Server) hCreateRuleset(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.repo(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var rs Ruleset
	_ = json.NewDecoder(r.Body).Decode(&rs)
	rs.ID = s.nextID()
	s.mu.Lock()
	rp.Rulesets = append(rp.Rulesets, &rs)
	s.mu.Unlock()
	writeJSON(w, 201, rs)
}

// --- GraphQL: only disablePullRequestAutoMerge ---

func (s *Server) hGraphQL(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if !strings.Contains(body.Query, "disablePullRequestAutoMerge") {
		writeJSON(w, 200, map[string]any{"errors": []any{map[string]any{"message": "ghfake: unsupported query"}}})
		return
	}
	nodeID, _ := body.Variables["id"].(string)
	s.mu.Lock()
	var found *PR
	for _, rp := range s.repos {
		for _, p := range rp.PRs {
			if p.NodeID == nodeID {
				p.AutoMerge = false
				found = p
			}
		}
	}
	s.mu.Unlock()
	if found == nil {
		writeJSON(w, 200, map[string]any{"errors": []any{map[string]any{"message": "Pull request auto merge is not enabled."}}})
		return
	}
	writeJSON(w, 200, map[string]any{"data": map[string]any{
		"disablePullRequestAutoMerge": map[string]any{"clientMutationId": nil},
	}})
}
