package store

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/AlphaTechini/newgit/internal/gitx"
	"github.com/AlphaTechini/newgit/internal/oplog"
)

// names caches id -> username without touching the main lock, so repo-level
// code can resolve names while holding a repo lock.
var names sync.Map

func nameOf(id string) string {
	if v, ok := names.Load(id); ok {
		return v.(string)
	}
	if id == "" {
		return "unknown"
	}
	return "user-" + id[:min(8, len(id))]
}

// ---------- op payloads ----------

type issueOpened struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}
type commentAdded struct {
	ID      string `json:"id"`
	Body    string `json:"body"`
	Version int    `json:"version,omitempty"`
}
type stateChanged struct {
	State string `json:"state"`
}
type mrOpened struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Source      string `json:"source,omitempty"`
	Target      string `json:"target"`
	Topic       string `json:"topic,omitempty"`
}
type mrVersion struct {
	N    int    `json:"n"`
	Head string `json:"head"`
	Base string `json:"base"`
}
type mrMerged struct {
	Commit string `json:"commit"`
}

// ---------- loading ----------

func (s *Store) loadRepo(ctx context.Context, rs *repoState) error {
	refs, err := gitx.ListRefs(ctx, rs.repo.Dir, "refs/newgit/")
	if err != nil {
		return err
	}
	for _, r := range refs {
		ops, err := oplog.ReadFrom(ctx, rs.repo.Dir, r.OID)
		if err != nil {
			return err
		}
		switch {
		case strings.HasPrefix(r.Name, "refs/newgit/issues/"):
			n, err := strconv.Atoi(strings.TrimPrefix(r.Name, "refs/newgit/issues/"))
			if err != nil {
				continue
			}
			is := &Issue{Number: n}
			for _, op := range ops {
				applyIssueOp(is, op)
			}
			rs.issues[n] = is
		case strings.HasPrefix(r.Name, "refs/newgit/mr/"):
			n, err := strconv.Atoi(strings.TrimPrefix(r.Name, "refs/newgit/mr/"))
			if err != nil {
				continue
			}
			mr := &MergeRequest{Number: n}
			for _, op := range ops {
				applyMROp(mr, op)
			}
			rs.mrs[n] = mr
		case r.Name == "refs/newgit/pushlog":
			for _, op := range ops {
				applyPushOp(rs, op)
			}
		}
	}
	return nil
}

func applyIssueOp(is *Issue, op oplog.Op) {
	who := nameOf(op.By)
	switch op.Type {
	case "issue.opened":
		var d issueOpened
		_ = op.Decode(&d)
		is.Title, is.Body, is.AuthorID, is.Author, is.Created, is.State = d.Title, d.Body, op.By, who, op.Time(), "opened"
	case "issue.comment":
		var d commentAdded
		_ = op.Decode(&d)
		c := Comment{ID: d.ID, AuthorID: op.By, Author: who, Body: d.Body, At: op.Time()}
		is.Comments = append(is.Comments, c)
		is.Timeline = append(is.Timeline, Event{Kind: "comment", Actor: who, At: op.Time(), Comment: &c})
	case "issue.state":
		var d stateChanged
		_ = op.Decode(&d)
		is.State = d.State
		kind := "closed"
		if d.State == "opened" {
			kind = "reopened"
		}
		is.Timeline = append(is.Timeline, Event{Kind: kind, Actor: who, At: op.Time()})
	}
	is.Updated = op.Time()
}

func applyMROp(mr *MergeRequest, op oplog.Op) {
	who := nameOf(op.By)
	switch op.Type {
	case "mr.opened":
		var d mrOpened
		_ = op.Decode(&d)
		mr.Title, mr.Description, mr.SourceBranch, mr.TargetBranch, mr.Topic = d.Title, d.Description, d.Source, d.Target, d.Topic
		mr.AuthorID, mr.Author, mr.Created, mr.State = op.By, who, op.Time(), "opened"
	case "mr.version":
		var d mrVersion
		_ = op.Decode(&d)
		mr.Versions = append(mr.Versions, MRVersion{N: d.N, Head: d.Head, Base: d.Base, PusherID: op.By, Pusher: who, At: op.Time()})
		detail := ""
		if d.N > 1 {
			detail = "pushed a new version"
		}
		mr.Timeline = append(mr.Timeline, Event{Kind: "version", Actor: who, At: op.Time(), Version: d.N, Detail: detail})
	case "mr.comment":
		var d commentAdded
		_ = op.Decode(&d)
		c := Comment{ID: d.ID, AuthorID: op.By, Author: who, Body: d.Body, At: op.Time(), Version: d.Version}
		mr.Comments = append(mr.Comments, c)
		mr.Timeline = append(mr.Timeline, Event{Kind: "comment", Actor: who, At: op.Time(), Comment: &c})
	case "mr.state":
		var d stateChanged
		_ = op.Decode(&d)
		mr.State = d.State
		kind := "closed"
		if d.State == "opened" {
			kind = "reopened"
		}
		mr.Timeline = append(mr.Timeline, Event{Kind: kind, Actor: who, At: op.Time()})
	case "mr.merged":
		var d mrMerged
		_ = op.Decode(&d)
		mr.State, mr.MergeCommit, mr.MergedBy = "merged", d.Commit, who
		mr.Timeline = append(mr.Timeline, Event{Kind: "merged", Actor: who, At: op.Time(), Detail: d.Commit})
	}
	mr.Updated = op.Time()
}

func applyPushOp(rs *repoState, op oplog.Op) {
	if op.Type != "push" {
		return
	}
	var p Push
	_ = op.Decode(&p)
	p.ByID = op.By
	p.By = nameOf(op.By)
	p.At = op.Time()
	rs.pushes = append(rs.pushes, p)
	idx := len(rs.pushes) - 1
	for _, c := range p.Commits {
		if _, seen := rs.pushOf[c]; !seen {
			rs.pushOf[c] = idx
		}
	}
}

// ---------- helpers ----------

func (s *Store) repoState(repoID string) (*repoState, error) {
	rs, ok := s.state(repoID)
	if !ok {
		return nil, ErrNotFound
	}
	return rs, nil
}

func (s *Store) append(ctx context.Context, rs *repoState, ref string, u User, typ string, data any) (oplog.Op, error) {
	op, err := oplog.NewOp(typ, u.ID, data)
	if err != nil {
		return op, err
	}
	if _, err := oplog.Append(ctx, rs.repo.Dir, ref, []oplog.Op{op}, u.Identity(), "-"); err != nil {
		return op, err
	}
	return op, nil
}

func cleanText(s string, max int) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if len(s) > max {
		s = s[:max]
	}
	return s
}

// ---------- issues ----------

// Issues lists issues filtered by state ("opened", "closed", "all").
func (s *Store) Issues(repoID, state string) []Issue {
	rs, err := s.repoState(repoID)
	if err != nil {
		return nil
	}
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	var out []Issue
	for _, is := range rs.issues {
		if state == "" || state == "all" || is.State == state {
			out = append(out, copyIssue(is))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number > out[j].Number })
	return out
}

// CountIssues returns the number of open issues.
func (s *Store) CountOpen(repoID string) (issues, mrs int) {
	rs, err := s.repoState(repoID)
	if err != nil {
		return 0, 0
	}
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	for _, is := range rs.issues {
		if is.State == "opened" {
			issues++
		}
	}
	for _, m := range rs.mrs {
		if m.State == "opened" {
			mrs++
		}
	}
	return
}

func copyIssue(is *Issue) Issue {
	c := *is
	c.Comments = append([]Comment(nil), is.Comments...)
	c.Timeline = append([]Event(nil), is.Timeline...)
	return c
}

// Issue returns one issue.
func (s *Store) Issue(repoID string, n int) (Issue, error) {
	rs, err := s.repoState(repoID)
	if err != nil {
		return Issue{}, err
	}
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	is, ok := rs.issues[n]
	if !ok {
		return Issue{}, ErrNotFound
	}
	return copyIssue(is), nil
}

// CreateIssue opens an issue.
func (s *Store) CreateIssue(ctx context.Context, repoID string, u User, title, body string) (Issue, error) {
	title = strings.TrimSpace(cleanText(title, 255))
	if title == "" {
		return Issue{}, fmt.Errorf("%w: title is required", ErrInvalid)
	}
	rs, err := s.repoState(repoID)
	if err != nil {
		return Issue{}, err
	}
	rs.writeMu.Lock()
	defer rs.writeMu.Unlock()
	rs.mu.RLock()
	n := 1
	for k := range rs.issues {
		if k >= n {
			n = k + 1
		}
	}
	rs.mu.RUnlock()
	op, err := s.append(ctx, rs, fmt.Sprintf("refs/newgit/issues/%d", n), u, "issue.opened", issueOpened{Title: title, Body: cleanText(body, 64*1024)})
	if err != nil {
		return Issue{}, err
	}
	is := &Issue{Number: n}
	applyIssueOp(is, op)
	rs.mu.Lock()
	rs.issues[n] = is
	c := copyIssue(is)
	rs.mu.Unlock()
	return c, nil
}

// CommentIssue adds a comment.
func (s *Store) CommentIssue(ctx context.Context, repoID string, n int, u User, body string) (Comment, error) {
	body = strings.TrimSpace(cleanText(body, 64*1024))
	if body == "" {
		return Comment{}, fmt.Errorf("%w: comment is empty", ErrInvalid)
	}
	rs, err := s.repoState(repoID)
	if err != nil {
		return Comment{}, err
	}
	rs.writeMu.Lock()
	defer rs.writeMu.Unlock()
	rs.mu.RLock()
	is, ok := rs.issues[n]
	rs.mu.RUnlock()
	if !ok {
		return Comment{}, ErrNotFound
	}
	op, err := s.append(ctx, rs, fmt.Sprintf("refs/newgit/issues/%d", n), u, "issue.comment", commentAdded{ID: randHex(4), Body: body})
	if err != nil {
		return Comment{}, err
	}
	rs.mu.Lock()
	applyIssueOp(is, op)
	c := is.Comments[len(is.Comments)-1]
	rs.mu.Unlock()
	return c, nil
}

// SetIssueState closes or reopens an issue (author or repo owner).
func (s *Store) SetIssueState(ctx context.Context, repoID string, n int, u User, state string) error {
	if state != "opened" && state != "closed" {
		return fmt.Errorf("%w: state must be opened or closed", ErrInvalid)
	}
	rs, err := s.repoState(repoID)
	if err != nil {
		return err
	}
	rs.writeMu.Lock()
	defer rs.writeMu.Unlock()
	rs.mu.RLock()
	is, ok := rs.issues[n]
	owner := rs.repo.OwnerID
	var author, cur string
	if ok {
		author, cur = is.AuthorID, is.State
	}
	rs.mu.RUnlock()
	if !ok {
		return ErrNotFound
	}
	if u.ID != author && u.ID != owner {
		return ErrForbidden
	}
	if cur == state {
		return nil
	}
	op, err := s.append(ctx, rs, fmt.Sprintf("refs/newgit/issues/%d", n), u, "issue.state", stateChanged{State: state})
	if err != nil {
		return err
	}
	rs.mu.Lock()
	applyIssueOp(is, op)
	rs.mu.Unlock()
	return nil
}

// ---------- merge requests ----------

func copyMR(m *MergeRequest) MergeRequest {
	c := *m
	c.Versions = append([]MRVersion(nil), m.Versions...)
	c.Comments = append([]Comment(nil), m.Comments...)
	c.Timeline = append([]Event(nil), m.Timeline...)
	return c
}

// MergeRequests lists merge requests filtered by state.
func (s *Store) MergeRequests(repoID, state string) []MergeRequest {
	rs, err := s.repoState(repoID)
	if err != nil {
		return nil
	}
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	var out []MergeRequest
	for _, m := range rs.mrs {
		if state == "" || state == "all" || m.State == state {
			out = append(out, copyMR(m))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number > out[j].Number })
	return out
}

// MergeRequest returns one merge request.
func (s *Store) MergeRequest(repoID string, n int) (MergeRequest, error) {
	rs, err := s.repoState(repoID)
	if err != nil {
		return MergeRequest{}, err
	}
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	m, ok := rs.mrs[n]
	if !ok {
		return MergeRequest{}, ErrNotFound
	}
	return copyMR(m), nil
}

func nextMRNumber(rs *repoState) int {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	n := 1
	for k := range rs.mrs {
		if k >= n {
			n = k + 1
		}
	}
	return n
}

// createMRLocked creates the MR entity and its first version. Caller holds writeMu.
func (s *Store) createMRLocked(ctx context.Context, rs *repoState, u User, d mrOpened, head string) (*MergeRequest, error) {
	n := nextMRNumber(rs)
	if d.Topic == "" && d.Source == "" {
		d.Topic = fmt.Sprintf("mr-%d", n)
	}
	op, err := s.append(ctx, rs, fmt.Sprintf("refs/newgit/mr/%d", n), u, "mr.opened", d)
	if err != nil {
		return nil, err
	}
	m := &MergeRequest{Number: n}
	applyMROp(m, op)
	rs.mu.Lock()
	rs.mrs[n] = m
	rs.mu.Unlock()
	if err := s.addVersionLocked(ctx, rs, m, head, u); err != nil {
		return m, err
	}
	return m, nil
}

// addVersionLocked records a new MR version: immutable refs/merge-requests/N/vK,
// moves refs/merge-requests/N/head, and appends an mr.version op.
func (s *Store) addVersionLocked(ctx context.Context, rs *repoState, m *MergeRequest, head string, u User) error {
	rs.mu.RLock()
	k := len(m.Versions) + 1
	prev := ""
	if k > 1 {
		prev = m.Versions[k-2].Head
	}
	target := m.TargetBranch
	n := m.Number
	rs.mu.RUnlock()
	if prev == head {
		return nil
	}
	dir := rs.repo.Dir
	base := ""
	if tip, ok := gitx.RefTarget(ctx, dir, "refs/heads/"+target); ok {
		base, _ = gitx.MergeBase(ctx, dir, tip, head)
	}
	if err := gitx.UpdateRef(ctx, dir, fmt.Sprintf("refs/merge-requests/%d/v%d", n, k), head, "", nil); err != nil {
		return err
	}
	headRef := fmt.Sprintf("refs/merge-requests/%d/head", n)
	cur, _ := gitx.RefTarget(ctx, dir, headRef)
	if err := gitx.UpdateRef(ctx, dir, headRef, head, cur, nil); err != nil {
		return err
	}
	op, err := s.append(ctx, rs, fmt.Sprintf("refs/newgit/mr/%d", n), u, "mr.version", mrVersion{N: k, Head: head, Base: base})
	if err != nil {
		return err
	}
	rs.mu.Lock()
	applyMROp(m, op)
	rs.mu.Unlock()
	return nil
}

// CreateMergeRequest opens an MR from a source branch (repo owner only).
func (s *Store) CreateMergeRequest(ctx context.Context, repoID string, u User, title, description, source, target string) (MergeRequest, error) {
	rs, err := s.repoState(repoID)
	if err != nil {
		return MergeRequest{}, err
	}
	if !rs.repo.CanWrite(u.ID) {
		return MergeRequest{}, fmt.Errorf("%w: only the owner can open merge requests from branches; others can push to refs/for/%s", ErrForbidden, target)
	}
	title = strings.TrimSpace(cleanText(title, 255))
	if source == target {
		return MergeRequest{}, fmt.Errorf("%w: source and target must differ", ErrInvalid)
	}
	dir := rs.repo.Dir
	head, ok := gitx.RefTarget(ctx, dir, "refs/heads/"+source)
	if !ok || !gitx.SafeRevision(source) {
		return MergeRequest{}, fmt.Errorf("%w: source branch %q not found", ErrInvalid, source)
	}
	tgt, ok := gitx.RefTarget(ctx, dir, "refs/heads/"+target)
	if !ok || !gitx.SafeRevision(target) {
		return MergeRequest{}, fmt.Errorf("%w: target branch %q not found", ErrInvalid, target)
	}
	if gitx.IsAncestor(ctx, dir, head, tgt) {
		return MergeRequest{}, fmt.Errorf("%w: %s has no commits that are not already in %s", ErrInvalid, source, target)
	}
	if title == "" {
		if c, err := gitx.GetCommit(ctx, dir, head); err == nil {
			title = c.Subject
		}
	}
	rs.writeMu.Lock()
	defer rs.writeMu.Unlock()
	rs.mu.RLock()
	for _, m := range rs.mrs {
		if m.State == "opened" && m.SourceBranch == source && m.TargetBranch == target {
			rs.mu.RUnlock()
			return MergeRequest{}, fmt.Errorf("%w: merge request !%d is already open for %s", ErrExists, m.Number, source)
		}
	}
	rs.mu.RUnlock()
	m, err := s.createMRLocked(ctx, rs, u, mrOpened{Title: title, Description: cleanText(description, 64*1024), Source: source, Target: target}, head)
	if err != nil {
		return MergeRequest{}, err
	}
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return copyMR(m), nil
}

// CommentMergeRequest adds a comment tagged with the current version.
func (s *Store) CommentMergeRequest(ctx context.Context, repoID string, n int, u User, body string) (Comment, error) {
	body = strings.TrimSpace(cleanText(body, 64*1024))
	if body == "" {
		return Comment{}, fmt.Errorf("%w: comment is empty", ErrInvalid)
	}
	rs, err := s.repoState(repoID)
	if err != nil {
		return Comment{}, err
	}
	rs.writeMu.Lock()
	defer rs.writeMu.Unlock()
	rs.mu.RLock()
	m, ok := rs.mrs[n]
	v := 0
	if ok {
		v = len(m.Versions)
	}
	rs.mu.RUnlock()
	if !ok {
		return Comment{}, ErrNotFound
	}
	op, err := s.append(ctx, rs, fmt.Sprintf("refs/newgit/mr/%d", n), u, "mr.comment", commentAdded{ID: randHex(4), Body: body, Version: v})
	if err != nil {
		return Comment{}, err
	}
	rs.mu.Lock()
	applyMROp(m, op)
	c := m.Comments[len(m.Comments)-1]
	rs.mu.Unlock()
	return c, nil
}

// SetMergeRequestState closes or reopens an MR (author or owner).
func (s *Store) SetMergeRequestState(ctx context.Context, repoID string, n int, u User, state string) error {
	if state != "opened" && state != "closed" {
		return fmt.Errorf("%w: state must be opened or closed", ErrInvalid)
	}
	rs, err := s.repoState(repoID)
	if err != nil {
		return err
	}
	rs.writeMu.Lock()
	defer rs.writeMu.Unlock()
	rs.mu.RLock()
	m, ok := rs.mrs[n]
	var author, cur string
	if ok {
		author, cur = m.AuthorID, m.State
	}
	rs.mu.RUnlock()
	if !ok {
		return ErrNotFound
	}
	if u.ID != author && !rs.repo.CanWrite(u.ID) {
		return ErrForbidden
	}
	if cur == "merged" {
		return fmt.Errorf("%w: merge request is already merged", ErrInvalid)
	}
	if cur == state {
		return nil
	}
	op, err := s.append(ctx, rs, fmt.Sprintf("refs/newgit/mr/%d", n), u, "mr.state", stateChanged{State: state})
	if err != nil {
		return err
	}
	rs.mu.Lock()
	applyMROp(m, op)
	rs.mu.Unlock()
	return nil
}

// MergeStatus describes whether an MR can be merged.
type MergeStatus struct {
	Mergeable     bool
	AlreadyMerged bool
	Conflicts     []string
	TargetMissing bool
	Reason        string
}

// CheckMerge does a trial merge with merge-tree (no worktree).
func (s *Store) CheckMerge(ctx context.Context, repo Repo, m MergeRequest) MergeStatus {
	tgt, ok := gitx.RefTarget(ctx, repo.Dir, "refs/heads/"+m.TargetBranch)
	if !ok {
		return MergeStatus{TargetMissing: true, Reason: "target branch " + m.TargetBranch + " no longer exists"}
	}
	head := m.Latest().Head
	if head == "" {
		return MergeStatus{Reason: "no versions"}
	}
	if gitx.IsAncestor(ctx, repo.Dir, head, tgt) {
		return MergeStatus{AlreadyMerged: true, Reason: "all commits are already in " + m.TargetBranch}
	}
	res, err := gitx.MergeTree(ctx, repo.Dir, tgt, head)
	if err != nil {
		return MergeStatus{Reason: err.Error()}
	}
	if !res.Clean {
		return MergeStatus{Conflicts: res.Conflicts, Reason: "merge conflicts"}
	}
	return MergeStatus{Mergeable: true}
}

// Merge merges an MR into its target with a merge commit (owner only).
func (s *Store) Merge(ctx context.Context, repoID string, n int, u User, via string) (string, error) {
	rs, err := s.repoState(repoID)
	if err != nil {
		return "", err
	}
	if !rs.repo.CanWrite(u.ID) {
		return "", fmt.Errorf("%w: only the repository owner can merge", ErrForbidden)
	}
	rs.pushMu.Lock()
	defer rs.pushMu.Unlock()
	rs.writeMu.Lock()
	defer rs.writeMu.Unlock()
	rs.mu.RLock()
	mp, ok := rs.mrs[n]
	var m MergeRequest
	if ok {
		m = copyMR(mp)
	}
	rs.mu.RUnlock()
	if !ok {
		return "", ErrNotFound
	}
	if m.State != "opened" {
		return "", fmt.Errorf("%w: merge request is %s", ErrInvalid, m.State)
	}
	dir := rs.repo.Dir
	tgt, ok := gitx.RefTarget(ctx, dir, "refs/heads/"+m.TargetBranch)
	if !ok {
		return "", fmt.Errorf("%w: target branch missing", ErrInvalid)
	}
	head := m.Latest().Head
	if gitx.IsAncestor(ctx, dir, head, tgt) {
		return "", fmt.Errorf("%w: nothing to merge; all commits are already in %s", ErrInvalid, m.TargetBranch)
	}
	res, err := gitx.MergeTree(ctx, dir, tgt, head)
	if err != nil {
		return "", err
	}
	if !res.Clean {
		return "", fmt.Errorf("%w: merge conflicts in %s", ErrInvalid, strings.Join(res.Conflicts, ", "))
	}
	src := m.SourceBranch
	if src == "" {
		src = "refs/merge-requests/" + strconv.Itoa(n) + "/head"
	}
	msg := fmt.Sprintf("Merge branch '%s' into '%s'\n\n%s\n\nSee merge request !%d\n", src, m.TargetBranch, m.Title, n)
	commit, err := gitx.CommitTree(ctx, dir, res.Tree, []string{tgt, head}, msg, u.Identity(), nil)
	if err != nil {
		return "", err
	}
	if err := gitx.UpdateRef(ctx, dir, "refs/heads/"+m.TargetBranch, commit, tgt, nil); err != nil {
		return "", fmt.Errorf("target branch moved during merge, try again: %w", err)
	}
	s.recordPushLocked(ctx, rs, u, via, []RefUpdate{{Ref: "refs/heads/" + m.TargetBranch, Old: tgt, New: commit}}, []string{commit})
	op, err := s.append(ctx, rs, fmt.Sprintf("refs/newgit/mr/%d", n), u, "mr.merged", mrMerged{Commit: commit})
	if err != nil {
		return commit, err
	}
	rs.mu.Lock()
	applyMROp(mp, op)
	rs.mu.Unlock()
	s.MarkDirty(repoID)
	return commit, nil
}

// ---------- push log ----------

// PushLock serialises pushes to a repo; call the returned func to unlock.
func (s *Store) PushLock(repoID string) (func(), error) {
	rs, err := s.repoState(repoID)
	if err != nil {
		return nil, err
	}
	rs.pushMu.Lock()
	return rs.pushMu.Unlock, nil
}

// SnapshotRefs returns branch, tag and MR head refs.
func SnapshotRefs(ctx context.Context, dir string) map[string]string {
	refs, _ := gitx.ListRefs(ctx, dir, "refs/heads/", "refs/tags/", "refs/merge-requests/")
	m := map[string]string{}
	for _, r := range refs {
		if strings.HasPrefix(r.Name, "refs/merge-requests/") && !strings.HasSuffix(r.Name, "/head") {
			continue
		}
		m[r.Name] = r.OID
	}
	return m
}

// RecordPush records the result of a git push and updates branch-based MRs.
// Caller holds the push lock.
func (s *Store) RecordPush(ctx context.Context, repoID string, u User, via string, before, after map[string]string) {
	rs, err := s.repoState(repoID)
	if err != nil {
		return
	}
	var updates []RefUpdate
	var newTips, oldTips []string
	for ref, old := range before {
		oldTips = append(oldTips, old)
		if nw, ok := after[ref]; !ok {
			updates = append(updates, RefUpdate{Ref: ref, Old: old, New: gitx.ZeroOID})
		} else if nw != old {
			updates = append(updates, RefUpdate{Ref: ref, Old: old, New: nw})
			newTips = append(newTips, nw)
		}
	}
	for ref, nw := range after {
		if _, ok := before[ref]; !ok {
			updates = append(updates, RefUpdate{Ref: ref, Old: gitx.ZeroOID, New: nw})
			newTips = append(newTips, nw)
		}
	}
	if len(updates) == 0 {
		return
	}
	sort.Slice(updates, func(i, j int) bool { return updates[i].Ref < updates[j].Ref })
	commits, _ := gitx.RevList(ctx, rs.repo.Dir, newTips, oldTips)
	rs.writeMu.Lock()
	defer rs.writeMu.Unlock()
	s.recordPushLocked(ctx, rs, u, via, updates, commits)
	// Branch-based MRs get a new version when their source branch moves.
	for _, up := range updates {
		if !strings.HasPrefix(up.Ref, "refs/heads/") || up.New == gitx.ZeroOID {
			continue
		}
		branch := strings.TrimPrefix(up.Ref, "refs/heads/")
		rs.mu.RLock()
		var targets []*MergeRequest
		for _, m := range rs.mrs {
			if m.State == "opened" && m.SourceBranch == branch {
				targets = append(targets, m)
			}
		}
		rs.mu.RUnlock()
		for _, m := range targets {
			_ = s.addVersionLocked(ctx, rs, m, up.New, u)
		}
	}
	s.MarkDirty(repoID)
}

const maxPushCommits = 2000

func (s *Store) recordPushLocked(ctx context.Context, rs *repoState, u User, via string, updates []RefUpdate, commits []string) {
	if len(commits) > maxPushCommits {
		commits = commits[:maxPushCommits]
	}
	p := Push{Via: via, Refs: updates, Commits: commits}
	op, err := s.append(ctx, rs, "refs/newgit/pushlog", u, "push", p)
	if err != nil {
		return
	}
	rs.mu.Lock()
	applyPushOp(rs, op)
	rs.mu.Unlock()
}

// PusherOf returns the push that introduced a commit.
func (s *Store) PusherOf(repoID, commit string) (Push, bool) {
	rs, err := s.repoState(repoID)
	if err != nil {
		return Push{}, false
	}
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	i, ok := rs.pushOf[commit]
	if !ok {
		return Push{}, false
	}
	p := rs.pushes[i]
	p.Commits = nil
	return p, true
}

// RecentPushes returns the last n pushes, newest first.
func (s *Store) RecentPushes(repoID string, n int) []Push {
	rs, err := s.repoState(repoID)
	if err != nil {
		return nil
	}
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	var out []Push
	for i := len(rs.pushes) - 1; i >= 0 && len(out) < n; i-- {
		p := rs.pushes[i]
		p.Commits = append([]string(nil), p.Commits...)
		out = append(out, p)
	}
	return out
}
