// Package store holds NewGit's state. The source of truth is git itself:
// users, tokens and the repo registry live as op-log entities in
// data/system.git, and issues, merge requests and the push log live as op-log
// entities inside each repository. Everything in memory here is an index
// rebuilt from those refs on startup.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AlphaTechini/newgit/internal/gitx"
	"github.com/AlphaTechini/newgit/internal/oplog"
)

// Errors returned by the store.
var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("forbidden")
	ErrExists    = errors.New("already exists")
	ErrInvalid   = errors.New("invalid input")
)

// Store is the application state.
type Store struct {
	DataDir   string
	SystemDir string
	ReposDir  string

	mu         sync.RWMutex
	users      map[string]*User
	byName     map[string]string // lower(username) -> id
	tokens     map[string]tokenRef
	repos      map[string]*repoState
	repoByPath map[string]string // lower(owner/name) -> id
	sysMu      sync.Mutex        // serialises system.git writes

	maintMu    sync.Mutex
	maintDirty map[string]int
}

type tokenRef struct {
	userID  string
	tokenID string
}

type repoState struct {
	mu      sync.RWMutex // guards fields below
	writeMu sync.Mutex   // serialises op-log writes and ref updates made by the server
	pushMu  sync.Mutex   // serialises pushes (so before/after snapshots are exact)
	repo    Repo
	issues  map[int]*Issue
	mrs     map[int]*MergeRequest
	pushes  []Push
	pushOf  map[string]int // commit -> index into pushes
	created bool
}

// Open opens (or initialises) the data directory and rebuilds the index.
func Open(ctx context.Context, dataDir string) (*Store, error) {
	s := &Store{
		DataDir:    dataDir,
		SystemDir:  filepath.Join(dataDir, "system.git"),
		ReposDir:   filepath.Join(dataDir, "repos"),
		users:      map[string]*User{},
		byName:     map[string]string{},
		tokens:     map[string]tokenRef{},
		repos:      map[string]*repoState{},
		repoByPath: map[string]string{},
		maintDirty: map[string]int{},
	}
	if err := os.MkdirAll(s.ReposDir, 0o750); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(s.SystemDir, "HEAD")); err != nil {
		if _, err := gitx.Run(ctx, "", nil, "init", "--bare", "-q", s.SystemDir); err != nil {
			return nil, err
		}
	}
	if err := s.rebuild(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) rebuild(ctx context.Context) error {
	start := time.Now()
	refs, err := gitx.ListRefs(ctx, s.SystemDir, "refs/newgit/users/", "refs/newgit/repos/")
	if err != nil {
		return err
	}
	for _, r := range refs {
		ops, err := oplog.ReadFrom(ctx, s.SystemDir, r.OID)
		if err != nil {
			return err
		}
		switch {
		case strings.HasPrefix(r.Name, "refs/newgit/users/"):
			u := applyUserOps(strings.TrimPrefix(r.Name, "refs/newgit/users/"), ops)
			s.indexUser(u)
		}
	}
	for _, r := range refs {
		if !strings.HasPrefix(r.Name, "refs/newgit/repos/") {
			continue
		}
		ops, err := oplog.ReadFrom(ctx, s.SystemDir, r.OID)
		if err != nil {
			return err
		}
		repo := s.applyRepoOps(strings.TrimPrefix(r.Name, "refs/newgit/repos/"), ops)
		if repo == nil {
			continue
		}
		rs := &repoState{repo: *repo, issues: map[int]*Issue{}, mrs: map[int]*MergeRequest{}, pushOf: map[string]int{}, created: true}
		if err := s.loadRepo(ctx, rs); err != nil {
			log.Printf("index: repo %s: %v", repo.Path(), err)
		}
		s.repos[repo.ID] = rs
		s.repoByPath[strings.ToLower(repo.Path())] = repo.ID
	}
	log.Printf("index rebuilt from git refs: %d users, %d repos in %s", len(s.users), len(s.repos), time.Since(start).Round(time.Millisecond))
	return nil
}

// ---------- users ----------

type userCreated struct {
	Username string `json:"username"`
	KeyX     string `json:"x"`
	KeyY     string `json:"y"`
}

type tokenAdded struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Hash  string `json:"hash"`
	Scope string `json:"scope"`
}

type tokenRevoked struct {
	ID string `json:"id"`
}

func applyUserOps(id string, ops []oplog.Op) *User {
	u := &User{ID: id}
	for _, op := range ops {
		switch op.Type {
		case "user.created":
			var d userCreated
			_ = op.Decode(&d)
			u.Username, u.KeyX, u.KeyY, u.Created = d.Username, d.KeyX, d.KeyY, op.Time()
		case "token.added":
			var d tokenAdded
			_ = op.Decode(&d)
			u.Tokens = append(u.Tokens, Token{ID: d.ID, Name: d.Name, Hash: d.Hash, Scope: d.Scope, Created: op.Time()})
		case "token.revoked":
			var d tokenRevoked
			_ = op.Decode(&d)
			for i := range u.Tokens {
				if u.Tokens[i].ID == d.ID {
					u.Tokens[i].Revoked = true
				}
			}
		}
	}
	return u
}

func (s *Store) indexUser(u *User) {
	s.users[u.ID] = u
	s.byName[strings.ToLower(u.Username)] = u.ID
	names.Store(u.ID, u.Username)
	for _, t := range u.Tokens {
		if !t.Revoked {
			s.tokens[t.Hash] = tokenRef{userID: u.ID, tokenID: t.ID}
		} else {
			delete(s.tokens, t.Hash)
		}
	}
}

// UsernameRe is the allowed username pattern.
var UsernameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$`)

var reservedNames = map[string]bool{"admin": true, "api": true, "explore": true, "new": true, "mcp": true, "static": true, "device": true, "login": true, "root": true, "system": true, "newgit": true, "help": true}

// Identity returns the git identity used for server-made commits by u.
func (u User) Identity() gitx.Identity {
	return gitx.Identity{Name: u.Username, Email: u.Username + "@users.newgit"}
}

func sysIdentity() gitx.Identity { return gitx.Identity{Name: "NewGit", Email: "system@newgit"} }

// CreateUser registers a device-bound account.
func (s *Store) CreateUser(ctx context.Context, id, username, x, y string) (*User, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if !UsernameRe.MatchString(username) || reservedNames[username] || strings.Contains(username, "--") {
		return nil, fmt.Errorf("%w: username must be 3-32 chars of a-z, 0-9 and single dashes", ErrInvalid)
	}
	s.sysMu.Lock()
	defer s.sysMu.Unlock()
	s.mu.RLock()
	_, idTaken := s.users[id]
	_, nameTaken := s.byName[username]
	s.mu.RUnlock()
	if idTaken {
		return nil, fmt.Errorf("%w: this device already has an account", ErrExists)
	}
	if nameTaken {
		return nil, fmt.Errorf("%w: username taken", ErrExists)
	}
	op, err := oplog.NewOp("user.created", id, userCreated{Username: username, KeyX: x, KeyY: y})
	if err != nil {
		return nil, err
	}
	u := &User{ID: id, Username: username, KeyX: x, KeyY: y, Created: op.Time()}
	if _, err := oplog.Append(ctx, s.SystemDir, "refs/newgit/users/"+id, []oplog.Op{op}, u.Identity(), ""); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.indexUser(u)
	s.mu.Unlock()
	return u, nil
}

// UserByID returns a copy of a user.
func (s *Store) UserByID(id string) (User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[id]
	if !ok {
		return User{}, false
	}
	c := *u
	c.Tokens = append([]Token(nil), u.Tokens...)
	return c, true
}

// UserByName returns a user by username.
func (s *Store) UserByName(name string) (User, bool) {
	s.mu.RLock()
	id, ok := s.byName[strings.ToLower(name)]
	s.mu.RUnlock()
	if !ok {
		return User{}, false
	}
	return s.UserByID(id)
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// HashToken returns the stored hash of a token.
func HashToken(plain string) string {
	h := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(h[:])
}

// CreateToken issues a personal access token and returns its plaintext once.
func (s *Store) CreateToken(ctx context.Context, userID, name, scope string) (string, Token, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 60 {
		return "", Token{}, fmt.Errorf("%w: token name required (max 60 chars)", ErrInvalid)
	}
	if scope != "read" && scope != "write" {
		return "", Token{}, fmt.Errorf("%w: scope must be read or write", ErrInvalid)
	}
	u, ok := s.UserByID(userID)
	if !ok {
		return "", Token{}, ErrNotFound
	}
	plain := "ngp_" + randHex(24)
	t := Token{ID: randHex(6), Name: name, Hash: HashToken(plain), Scope: scope}
	s.sysMu.Lock()
	defer s.sysMu.Unlock()
	op, _ := oplog.NewOp("token.added", userID, tokenAdded{ID: t.ID, Name: t.Name, Hash: t.Hash, Scope: t.Scope})
	t.Created = op.Time()
	if _, err := oplog.Append(ctx, s.SystemDir, "refs/newgit/users/"+userID, []oplog.Op{op}, u.Identity(), "-"); err != nil {
		return "", Token{}, err
	}
	s.mu.Lock()
	s.users[userID].Tokens = append(s.users[userID].Tokens, t)
	s.tokens[t.Hash] = tokenRef{userID: userID, tokenID: t.ID}
	s.mu.Unlock()
	return plain, t, nil
}

// RevokeToken revokes a token.
func (s *Store) RevokeToken(ctx context.Context, userID, tokenID string) error {
	u, ok := s.UserByID(userID)
	if !ok {
		return ErrNotFound
	}
	found := false
	for _, t := range u.Tokens {
		if t.ID == tokenID && !t.Revoked {
			found = true
		}
	}
	if !found {
		return ErrNotFound
	}
	s.sysMu.Lock()
	defer s.sysMu.Unlock()
	op, _ := oplog.NewOp("token.revoked", userID, tokenRevoked{ID: tokenID})
	if _, err := oplog.Append(ctx, s.SystemDir, "refs/newgit/users/"+userID, []oplog.Op{op}, u.Identity(), "-"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.users[userID].Tokens {
		t := &s.users[userID].Tokens[i]
		if t.ID == tokenID {
			t.Revoked = true
			delete(s.tokens, t.Hash)
		}
	}
	return nil
}

// VerifyToken checks a plaintext token.
func (s *Store) VerifyToken(plain string) (User, Token, bool) {
	if !strings.HasPrefix(plain, "ngp_") {
		return User{}, Token{}, false
	}
	h := HashToken(plain)
	s.mu.RLock()
	ref, ok := s.tokens[h]
	s.mu.RUnlock()
	if !ok {
		return User{}, Token{}, false
	}
	u, ok := s.UserByID(ref.userID)
	if !ok {
		return User{}, Token{}, false
	}
	for _, t := range u.Tokens {
		if t.ID == ref.tokenID && !t.Revoked {
			return u, t, true
		}
	}
	return User{}, Token{}, false
}

// ---------- repos ----------

type repoCreated struct {
	OwnerID     string `json:"owner_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type repoUpdated struct {
	Description string `json:"description"`
}

func (s *Store) applyRepoOps(id string, ops []oplog.Op) *Repo {
	var r *Repo
	for _, op := range ops {
		switch op.Type {
		case "repo.created":
			var d repoCreated
			_ = op.Decode(&d)
			r = &Repo{ID: id, OwnerID: d.OwnerID, Name: d.Name, Description: d.Description, Created: op.Time(), Dir: filepath.Join(s.ReposDir, id+".git")}
			if u, ok := s.users[d.OwnerID]; ok {
				r.Owner = u.Username
			}
		case "repo.updated":
			if r != nil {
				var d repoUpdated
				_ = op.Decode(&d)
				r.Description = d.Description
			}
		}
	}
	return r
}

// RepoNameRe is the allowed repository name pattern.
var RepoNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidRepoName validates a repository name.
func ValidRepoName(name string) bool {
	return RepoNameRe.MatchString(name) && !strings.HasSuffix(strings.ToLower(name), ".git") && !strings.Contains(name, "..")
}

// CreateRepo creates a bare repository owned by the user.
func (s *Store) CreateRepo(ctx context.Context, owner User, name, description string) (Repo, error) {
	name = strings.TrimSpace(name)
	if !ValidRepoName(name) {
		return Repo{}, fmt.Errorf("%w: repository name must be 1-64 chars of letters, digits, '.', '_' or '-' and must not end in .git", ErrInvalid)
	}
	if len(description) > 500 {
		description = description[:500]
	}
	s.sysMu.Lock()
	defer s.sysMu.Unlock()
	path := strings.ToLower(owner.Username + "/" + name)
	s.mu.RLock()
	_, exists := s.repoByPath[path]
	s.mu.RUnlock()
	if exists {
		return Repo{}, fmt.Errorf("%w: you already have a repository named %s", ErrExists, name)
	}
	id := randHex(8)
	dir := filepath.Join(s.ReposDir, id+".git")
	if _, err := gitx.Run(ctx, "", nil, "init", "--bare", "-q", "-b", "main", dir); err != nil {
		return Repo{}, err
	}
	op, _ := oplog.NewOp("repo.created", owner.ID, repoCreated{OwnerID: owner.ID, Name: name, Description: description})
	if _, err := oplog.Append(ctx, s.SystemDir, "refs/newgit/repos/"+id, []oplog.Op{op}, owner.Identity(), ""); err != nil {
		_ = os.RemoveAll(dir)
		return Repo{}, err
	}
	r := Repo{ID: id, OwnerID: owner.ID, Owner: owner.Username, Name: name, Description: description, Created: op.Time(), Dir: dir}
	rs := &repoState{repo: r, issues: map[int]*Issue{}, mrs: map[int]*MergeRequest{}, pushOf: map[string]int{}, created: true}
	s.mu.Lock()
	s.repos[id] = rs
	s.repoByPath[path] = id
	s.mu.Unlock()
	return r, nil
}

func (s *Store) state(repoID string) (*repoState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rs, ok := s.repos[repoID]
	return rs, ok
}

// RepoByPath finds a repository by owner and name.
func (s *Store) RepoByPath(owner, name string) (Repo, bool) {
	s.mu.RLock()
	id, ok := s.repoByPath[strings.ToLower(owner+"/"+name)]
	var rs *repoState
	if ok {
		rs = s.repos[id]
	}
	s.mu.RUnlock()
	if !ok || rs == nil {
		return Repo{}, false
	}
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.repo, true
}

// RepoByID finds a repository by id.
func (s *Store) RepoByID(id string) (Repo, bool) {
	rs, ok := s.state(id)
	if !ok {
		return Repo{}, false
	}
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.repo, true
}

// ListRepos lists repositories, newest first. ownerID "" lists all.
func (s *Store) ListRepos(ownerID string) []Repo {
	s.mu.RLock()
	var out []Repo
	for _, rs := range s.repos {
		rs.mu.RLock()
		if ownerID == "" || rs.repo.OwnerID == ownerID {
			out = append(out, rs.repo)
		}
		rs.mu.RUnlock()
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// CanWrite reports whether user may push to / administer the repo.
func (r Repo) CanWrite(userID string) bool { return userID != "" && userID == r.OwnerID }

// ---------- maintenance ----------

// MarkDirty schedules commit-graph and repack maintenance for a repo.
func (s *Store) MarkDirty(repoID string) {
	s.maintMu.Lock()
	s.maintDirty[repoID] = needGraph | needRepack
	s.maintMu.Unlock()
}

const (
	needGraph  = 1
	needRepack = 2
)

// RunMaintenance writes commit-graphs (with changed-path Bloom filters) for
// repos changed since the last run, and when full is set also repacks them
// with reachability bitmaps.
func (s *Store) RunMaintenance(ctx context.Context, full bool) {
	s.maintMu.Lock()
	work := map[string]int{}
	for id, flags := range s.maintDirty {
		do := flags & needGraph
		if full {
			do |= flags & needRepack
		}
		if do != 0 {
			work[id] = do
		}
		if rest := flags &^ do; rest != 0 {
			s.maintDirty[id] = rest
		} else {
			delete(s.maintDirty, id)
		}
	}
	s.maintMu.Unlock()
	for id, do := range work {
		r, ok := s.RepoByID(id)
		if !ok {
			continue
		}
		if do&needRepack != 0 {
			if _, err := gitx.Run(ctx, r.Dir, nil, "repack", "-a", "-d", "-b", "-q"); err != nil {
				log.Printf("maintenance %s: repack: %v", r.Path(), err)
			}
		}
		if _, err := gitx.Run(ctx, r.Dir, nil, "commit-graph", "write", "--reachable", "--changed-paths"); err != nil {
			log.Printf("maintenance %s: commit-graph: %v", r.Path(), err)
		}
	}
}
