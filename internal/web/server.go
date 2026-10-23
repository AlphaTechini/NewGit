// Package web is NewGit's GitLab-style web UI and HTTP router.
package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/AlphaTechini/newgit/internal/auth"
	"github.com/AlphaTechini/newgit/internal/githttp"
	"github.com/AlphaTechini/newgit/internal/gitx"
	"github.com/AlphaTechini/newgit/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Server is the HTTP application.
type Server struct {
	Store   *store.Store
	Auth    *auth.Auth
	Git     *githttp.Server
	BaseURL string
	MCP     http.Handler

	pages     map[string]*template.Template
	chromaCSS string
}

type ctxKey int

const userKey ctxKey = 1

// New builds the server.
func New(st *store.Store, a *auth.Auth, g *githttp.Server, baseURL string, mcp http.Handler) (*Server, error) {
	s := &Server{Store: st, Auth: a, Git: g, BaseURL: strings.TrimRight(baseURL, "/"), MCP: mcp}
	if err := s.loadTemplates(); err != nil {
		return nil, err
	}
	s.chromaCSS = chromaCSS()
	return s, nil
}

func (s *Server) loadTemplates() error {
	s.pages = map[string]*template.Template{}
	entries, err := fs.ReadDir(assets, "templates")
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if name == "layout.html" || name == "partials.html" {
			continue
		}
		t, err := template.New(name).Funcs(funcMap(s)).ParseFS(assets, "templates/layout.html", "templates/partials.html", "templates/"+name)
		if err != nil {
			return fmt.Errorf("template %s: %w", name, err)
		}
		s.pages[name] = t
	}
	return nil
}

// Page is the data every template gets.
type Page struct {
	Title     string
	User      *store.User
	CSRF      string
	Repo      *RepoCtx
	Nav       string
	Flash     string // human message for the ?flash=<code> whitelist ("" when none/unknown)
	FlashCode string // the validated code itself, e.g. "created"
	Theme     string // "light", "dark" or "" (follow system), from cookie ng_theme
	Path      string // current request path+query without ?flash=, a safe value for the theme form's "next"
	Error     string
	BaseURL   string
	Data      any
	Now       time.Time
}

// RepoCtx is repository context for repo pages.
type RepoCtx struct {
	store.Repo
	Ref        string
	Default    string
	CloneURL   string
	OpenIssues int
	OpenMRs    int
	CanWrite   bool
	Empty      bool

	RefKind     string   // "branch", "tag" or "commit" for Ref
	Branches    []string // branch names, most recently committed first
	Tags        []string // tag names, newest first
	CommitCount int      // commits reachable from the default branch (0 if empty)
	BranchCount int
	TagCount    int
}

// flashMessages is the whitelist for ?flash=<code>. Unknown codes show nothing.
var flashMessages = map[string]string{
	"created":       "Project created.",
	"token_created": "Token created. Copy it now: it is shown only once.",
	"token_revoked": "Token revoked.",
	"signed_out":    "You have been signed out.",
	"merged":        "Merge request merged.",
	"issue_opened":  "Issue opened.",
	"mr_opened":     "Merge request opened.",
	"commented":     "Comment added.",
	"closed":        "Closed.",
	"reopened":      "Reopened.",
}

// withFlash appends ?flash=<code> to a local path (before any #fragment).
func withFlash(target, code string) string {
	frag := ""
	if i := strings.IndexByte(target, '#'); i >= 0 {
		target, frag = target[:i], target[i:]
	}
	sep := "?"
	if strings.Contains(target, "?") {
		sep = "&"
	}
	return target + sep + "flash=" + url.QueryEscape(code) + frag
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, p *Page) {
	t, ok := s.pages[name]
	if !ok {
		http.Error(w, "missing template "+name, http.StatusInternalServerError)
		return
	}
	s.fillPage(r, p)
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", p); err != nil {
		log.Printf("render %s: %v", name, err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) fillPage(r *http.Request, p *Page) {
	p.BaseURL = s.BaseURL
	p.Now = time.Now()
	if u := currentUser(r); u != nil {
		p.User = u
		if _, val, ok := s.Auth.SessionUser(r); ok {
			p.CSRF = s.Auth.CSRFToken(val)
		}
	}
	if p.Flash == "" {
		if msg, ok := flashMessages[r.URL.Query().Get("flash")]; ok {
			p.Flash, p.FlashCode = msg, r.URL.Query().Get("flash")
		}
	}
	u := *r.URL
	q := u.Query()
	q.Del("flash")
	u.RawQuery = q.Encode()
	p.Path = safeNext(u.RequestURI())
	if c, err := r.Cookie(themeCookie); err == nil && (c.Value == "light" || c.Value == "dark") {
		p.Theme = c.Value
	}
}

const themeCookie = "ng_theme"

// safeNext accepts only same-site relative paths.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n\x00") {
		return "/"
	}
	return next
}

// handleTheme sets the cosmetic theme cookie. Signed-in visitors must send a
// valid CSRF token (it is derived from the session cookie); signed-out
// visitors have no session and therefore no token, and the cookie is purely
// cosmetic (SameSite=Lax, no authority), so no CSRF check is made for them.
func (s *Server) handleTheme(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if _, val, ok := s.Auth.SessionUser(r); ok && !s.Auth.CheckCSRF(val, r.PostForm.Get("csrf")) {
		s.errorPage(w, r, 403, "Your session token did not match. Reload the page and try again.")
		return
	}
	c := &http.Cookie{Name: themeCookie, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: strings.HasPrefix(s.BaseURL, "https://")}
	switch r.PostForm.Get("theme") {
	case "light", "dark":
		c.Value = r.PostForm.Get("theme")
		c.MaxAge = 365 * 24 * 3600
	case "system":
		c.MaxAge = -1
	default:
		http.Error(w, "theme must be light, dark or system", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, c)
	http.Redirect(w, r, safeNext(r.PostForm.Get("next")), http.StatusSeeOther)
}

func currentUser(r *http.Request) *store.User {
	u, _ := r.Context().Value(userKey).(*store.User)
	return u
}

func (s *Server) errorPage(w http.ResponseWriter, r *http.Request, code int, msg string) {
	w.WriteHeader(code)
	s.render(w, r, "error.html", &Page{Title: http.StatusText(code), Data: map[string]any{"Code": code, "Message": msg}})
}

func (s *Server) storeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.errorPage(w, r, 404, "Not found.")
	case errors.Is(err, store.ErrForbidden):
		s.errorPage(w, r, 403, err.Error())
	case errors.Is(err, store.ErrInvalid), errors.Is(err, store.ErrExists):
		s.errorPage(w, r, 422, err.Error())
	default:
		log.Printf("error: %v", err)
		s.errorPage(w, r, 500, "Something went wrong.")
	}
}

// requireUser returns the signed-in user or redirects to sign-in.
func (s *Server) requireUser(w http.ResponseWriter, r *http.Request) *store.User {
	u := currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/-/signin?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return nil
	}
	return u
}

// checkPost validates method, session and CSRF for form posts.
func (s *Server) checkPost(w http.ResponseWriter, r *http.Request) *store.User {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return nil
	}
	u := currentUser(r)
	_, val, ok := s.Auth.SessionUser(r)
	if u == nil || !ok {
		http.Redirect(w, r, "/-/signin", http.StatusSeeOther)
		return nil
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return nil
	}
	if !s.Auth.CheckCSRF(val, r.PostForm.Get("csrf")) {
		s.errorPage(w, r, 403, "Your session token did not match. Reload the page and try again.")
		return nil
	}
	return u
}

// withUser attaches the session user to the request context.
func (s *Server) withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if uid, _, ok := s.Auth.SessionUser(r); ok {
			if u, ok := s.Store.UserByID(uid); ok {
				r = r.WithContext(context.WithValue(r.Context(), userKey, &u))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Handler returns the root handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	staticFS, _ := fs.Sub(assets, "static")
	static := http.StripPrefix("/-/static/", http.FileServer(http.FS(staticFS)))
	mux.HandleFunc("/-/static/chroma.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write([]byte(s.chromaCSS))
	})
	mux.Handle("/-/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/-/static/fonts/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=300")
		}
		static.ServeHTTP(w, r)
	}))
	mux.HandleFunc("/-/theme", s.handleTheme)
	mux.HandleFunc("/-/signin", s.handleSignin)
	mux.HandleFunc("/-/signout", s.handleSignout)
	mux.HandleFunc("/-/auth/challenge", s.handleChallenge)
	mux.HandleFunc("/-/auth/register", s.handleRegister)
	mux.HandleFunc("/-/auth/login", s.handleLogin)
	mux.HandleFunc("/-/device", s.handleDevice)
	mux.HandleFunc("/-/device/tokens", s.handleCreateToken)
	mux.HandleFunc("/-/device/tokens/revoke", s.handleRevokeToken)
	mux.HandleFunc("/-/new", s.handleNewRepo)
	mux.HandleFunc("/-/health", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("/explore", s.handleExplore)
	if s.MCP != nil {
		mux.Handle("/mcp", s.MCP)
		mux.Handle("/mcp/", s.MCP)
	}
	mux.HandleFunc("/", s.route)
	return s.withUser(securityHeaders(mux))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// route dispatches /, /<user>, /<owner>/<repo>[.git]/...
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	p := strings.Trim(r.URL.Path, "/")
	if p == "" {
		s.handleHome(w, r)
		return
	}
	parts := strings.SplitN(p, "/", 3)
	if len(parts) == 1 {
		s.handleUserPage(w, r, parts[0])
		return
	}
	owner, name := parts[0], parts[1]
	rest := ""
	if len(parts) == 3 {
		rest = parts[2]
	}
	isGitPath := strings.HasSuffix(name, ".git")
	name = strings.TrimSuffix(name, ".git")
	repo, ok := s.Store.RepoByPath(owner, name)
	if !ok {
		if githttp.IsGitRequest(rest) {
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		s.errorPage(w, r, 404, "Repository not found.")
		return
	}
	if githttp.IsGitRequest(rest) {
		s.Git.Serve(w, r, repo, rest)
		return
	}
	if isGitPath && rest == "" {
		http.Redirect(w, r, "/"+repo.Path(), http.StatusFound)
		return
	}
	if rest == "" {
		s.handleRepoHome(w, r, repo)
		return
	}
	if !strings.HasPrefix(rest, "-/") {
		s.errorPage(w, r, 404, "Page not found.")
		return
	}
	rest = strings.TrimPrefix(rest, "-/")
	section, sub, _ := strings.Cut(rest, "/")
	switch section {
	case "tree":
		s.handleTree(w, r, repo, sub)
	case "blob":
		s.handleBlob(w, r, repo, sub, false)
	case "raw":
		s.handleBlob(w, r, repo, sub, true)
	case "commits":
		s.handleCommits(w, r, repo, sub)
	case "commit":
		s.handleCommit(w, r, repo, sub)
	case "branches":
		s.handleBranches(w, r, repo)
	case "tags":
		s.handleTags(w, r, repo)
	case "pushes":
		s.handlePushes(w, r, repo)
	case "issues":
		s.handleIssues(w, r, repo, sub)
	case "merge_requests":
		s.handleMRs(w, r, repo, sub)
	case "settings":
		s.handleRepoSettings(w, r, repo)
	default:
		s.errorPage(w, r, 404, "Page not found.")
	}
}

func (s *Server) repoCtx(r *http.Request, repo store.Repo, ref string) *RepoCtx {
	ctx := r.Context()
	def := gitx.HeadBranch(ctx, repo.Dir)
	_, hasDef := gitx.RefTarget(ctx, repo.Dir, "refs/heads/"+def)
	empty := !hasDef
	if empty {
		if bs, _ := gitx.ListBranches(ctx, repo.Dir); len(bs) > 0 {
			empty = false
			def = bs[0].Name
		}
	}
	if ref == "" {
		ref = def
	}
	oi, om := s.Store.CountOpen(repo.ID)
	rc := &RepoCtx{Repo: repo, Ref: ref, Default: def, CloneURL: s.BaseURL + "/" + repo.Path() + ".git", OpenIssues: oi, OpenMRs: om, Empty: empty}
	rc.Branches, rc.Tags = []string{}, []string{}
	if bs, _ := gitx.ListBranches(ctx, repo.Dir); bs != nil {
		for _, b := range bs {
			rc.Branches = append(rc.Branches, b.Name)
		}
	}
	if ts, _ := gitx.ListTags(ctx, repo.Dir); ts != nil {
		for _, t := range ts {
			rc.Tags = append(rc.Tags, t.Name)
		}
	}
	rc.BranchCount, rc.TagCount = len(rc.Branches), len(rc.Tags)
	if !empty {
		rc.CommitCount, _ = gitx.CountCommits(ctx, repo.Dir, "refs/heads/"+def)
	}
	rc.RefKind = "commit"
	for _, b := range rc.Branches {
		if b == ref {
			rc.RefKind = "branch"
		}
	}
	if rc.RefKind == "commit" {
		for _, t := range rc.Tags {
			if t == ref {
				rc.RefKind = "tag"
			}
		}
	}
	if empty {
		rc.RefKind = "branch"
	}
	if u := currentUser(r); u != nil {
		rc.CanWrite = repo.CanWrite(u.ID)
	}
	return rc
}

// resolveRefPath splits "<ref>/<path>" where ref may contain slashes.
func resolveRefPath(ctx context.Context, dir, rest string) (ref, commit, p string, ok bool) {
	rest = strings.Trim(rest, "/")
	parts := strings.Split(rest, "/")
	for i := len(parts); i >= 1; i-- {
		cand := strings.Join(parts[:i], "/")
		if !gitx.SafeRevision(cand) {
			continue
		}
		for _, full := range []string{"refs/heads/" + cand, "refs/tags/" + cand} {
			if c, ok := gitx.RefTarget(ctx, dir, full+"^{commit}"); ok {
				return cand, c, strings.Join(parts[i:], "/"), true
			}
		}
		if len(cand) >= 7 && len(cand) <= 40 && isHex(cand) {
			if c, err := gitx.ResolveCommit(ctx, dir, cand); err == nil {
				return cand, c, strings.Join(parts[i:], "/"), true
			}
		}
	}
	return "", "", "", false
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func cleanRepoPath(p string) (string, bool) {
	if p == "" {
		return "", true
	}
	if strings.Contains(p, "\x00") || strings.HasPrefix(p, "/") {
		return "", false
	}
	c := path.Clean(p)
	if c != p || strings.HasPrefix(c, "..") {
		return "", false
	}
	return c, true
}

func atoi(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
