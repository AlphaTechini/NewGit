package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/AlphaTechini/newgit/internal/auth"
	"github.com/AlphaTechini/newgit/internal/store"
)

func (s *Server) handleSignin(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Query().Get("next")
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/"
	}
	if currentUser(r) != nil {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	s.render(w, r, "signin.html", &Page{Title: "Sign in with this device", Data: map[string]any{"Next": next}})
}

func (s *Server) handleSignout(w http.ResponseWriter, r *http.Request) {
	if s.checkPost(w, r) == nil {
		return
	}
	s.Auth.ClearSession(w)
	http.Redirect(w, r, "/?signedout=1", http.StatusSeeOther)
}

func (s *Server) handleChallenge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]string{"nonce": s.Auth.NewChallenge()})
}

type registerReq struct {
	Username string `json:"username"`
	X        string `json:"x"`
	Y        string `json:"y"`
	Nonce    string `json:"nonce"`
	Sig      string `json:"sig"`
}

type loginReq struct {
	ID    string `json:"id"`
	Nonce string `json:"nonce"`
	Sig   string `json:"sig"`
}

func jsonPost(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "POST only"})
		return false
	}
	// Requiring a JSON content type forces a CORS preflight for cross-site
	// requests, so other origins cannot drive these endpoints.
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeJSON(w, 415, map[string]string{"error": "JSON required"})
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad JSON"})
		return false
	}
	return true
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerReq
	if !jsonPost(w, r, &req) {
		return
	}
	pub, err := auth.ParseKey(req.X, req.Y)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if err := s.Auth.VerifyProof(pub, "register", req.Nonce, req.Sig); err != nil {
		writeJSON(w, 401, map[string]string{"error": err.Error()})
		return
	}
	id := auth.Thumbprint(req.X, req.Y)
	u, err := s.Store.CreateUser(r.Context(), id, req.Username, req.X, req.Y)
	if err != nil {
		code := 500
		if errors.Is(err, store.ErrInvalid) || errors.Is(err, store.ErrExists) {
			code = 422
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	s.Auth.SetSession(w, u.ID)
	writeJSON(w, 200, map[string]string{"id": u.ID, "username": u.Username})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if !jsonPost(w, r, &req) {
		return
	}
	u, ok := s.Store.UserByID(req.ID)
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "unknown device", "code": "unknown_device"})
		return
	}
	pub, err := auth.ParseKey(u.KeyX, u.KeyY)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "stored key invalid"})
		return
	}
	if err := s.Auth.VerifyProof(pub, "login", req.Nonce, req.Sig); err != nil {
		writeJSON(w, 401, map[string]string{"error": err.Error()})
		return
	}
	s.Auth.SetSession(w, u.ID)
	writeJSON(w, 200, map[string]string{"id": u.ID, "username": u.Username})
}

func (s *Server) handleDevice(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	newToken := ""
	if c, err := r.Cookie("ng_newtoken"); err == nil {
		newToken = c.Value
		http.SetCookie(w, &http.Cookie{Name: "ng_newtoken", Path: "/-/device", MaxAge: -1, HttpOnly: true})
	}
	s.render(w, r, "device.html", &Page{Title: "This device", Nav: "device", Data: map[string]any{
		"Tokens": u.Tokens, "NewToken": newToken, "MCPURL": s.BaseURL + "/mcp",
		"Repos": s.Store.ListRepos(u.ID),
	}})
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	u := s.checkPost(w, r)
	if u == nil {
		return
	}
	plain, _, err := s.Store.CreateToken(r.Context(), u.ID, r.PostForm.Get("name"), r.PostForm.Get("scope"))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	// Show the token exactly once, via a short-lived HttpOnly cookie read by
	// the next page load (so it never appears in a URL or the history).
	http.SetCookie(w, &http.Cookie{Name: "ng_newtoken", Value: plain, Path: "/-/device", MaxAge: 120, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/-/device", http.StatusSeeOther)
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	u := s.checkPost(w, r)
	if u == nil {
		return
	}
	if err := s.Store.RevokeToken(r.Context(), u.ID, r.PostForm.Get("id")); err != nil {
		s.storeError(w, r, err)
		return
	}
	http.Redirect(w, r, "/-/device?flash=Token+revoked", http.StatusSeeOther)
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		s.handleExplore(w, r)
		return
	}
	s.render(w, r, "home.html", &Page{Title: "Projects", Nav: "home", Data: map[string]any{
		"Mine": s.Store.ListRepos(u.ID), "All": s.Store.ListRepos(""),
	}})
}

func (s *Server) handleExplore(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "explore.html", &Page{Title: "Explore projects", Nav: "explore", Data: map[string]any{
		"All": s.Store.ListRepos(""),
	}})
}

func (s *Server) handleUserPage(w http.ResponseWriter, r *http.Request, name string) {
	u, ok := s.Store.UserByName(name)
	if !ok {
		s.errorPage(w, r, 404, "User not found.")
		return
	}
	s.render(w, r, "user.html", &Page{Title: u.Username, Data: map[string]any{"Profile": u, "Repos": s.Store.ListRepos(u.ID)}})
}

func (s *Server) handleNewRepo(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if r.Method == http.MethodGet {
		s.render(w, r, "new_repo.html", &Page{Title: "New project", Nav: "home"})
		return
	}
	if s.checkPost(w, r) == nil {
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("name"))
	desc := strings.TrimSpace(r.PostForm.Get("description"))
	repo, err := s.Store.CreateRepo(r.Context(), *u, name, desc)
	if err != nil {
		if errors.Is(err, store.ErrInvalid) || errors.Is(err, store.ErrExists) {
			s.render(w, r, "new_repo.html", &Page{Title: "New project", Nav: "home", Error: err.Error(), Data: map[string]any{"Name": name, "Description": desc}})
			return
		}
		s.storeError(w, r, err)
		return
	}
	if r.PostForm.Get("readme") == "on" {
		content := "# " + repo.Name + "\n"
		if desc != "" {
			content += "\n" + desc + "\n"
		}
		_, _ = s.Store.CommitFiles(r.Context(), repo.ID, *u, "web", "main", "", "Initial commit", []store.FileChange{{Path: "README.md", Content: content}})
	}
	http.Redirect(w, r, "/"+repo.Path(), http.StatusSeeOther)
}

func (s *Server) handleRepoSettings(w http.ResponseWriter, r *http.Request, repo store.Repo) {
	s.errorPage(w, r, 404, "Settings are not available in this version.")
}
