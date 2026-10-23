package store

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/AlphaTechini/newgit/internal/gitx"
)

// AGitCommand is one refs/for/* command received by proc-receive.
type AGitCommand struct {
	Old string `json:"old"`
	New string `json:"new"`
	Ref string `json:"ref"`
}

// AGitResult is the outcome for one command.
type AGitResult struct {
	Ref     string `json:"ref"`
	OK      bool   `json:"ok"`
	Reason  string `json:"reason,omitempty"`
	RefName string `json:"refname,omitempty"`
	OldOID  string `json:"old_oid,omitempty"`
	NewOID  string `json:"new_oid,omitempty"`
	Forced  bool   `json:"forced,omitempty"`
	Message string `json:"message,omitempty"`
	Number  int    `json:"number,omitempty"`
}

// HandleAGit turns `git push origin HEAD:refs/for/<target>` into merge
// requests. Pushing again with the same -o topic=<topic> adds a version.
func (s *Store) HandleAGit(ctx context.Context, repoID string, u User, cmds []AGitCommand, opts map[string]string, baseURL string) []AGitResult {
	rs, err := s.repoState(repoID)
	out := make([]AGitResult, 0, len(cmds))
	if err != nil {
		for _, c := range cmds {
			out = append(out, AGitResult{Ref: c.Ref, Reason: "repository not found"})
		}
		return out
	}
	dir := rs.repo.Dir
	rs.writeMu.Lock()
	defer rs.writeMu.Unlock()
	for _, c := range cmds {
		res := AGitResult{Ref: c.Ref}
		if c.New == gitx.ZeroOID {
			res.Reason = "cannot delete a refs/for/ ref"
			out = append(out, res)
			continue
		}
		rest := strings.TrimPrefix(c.Ref, "refs/for/")
		topic := opts["topic"]
		target := rest
		if _, ok := gitx.RefTarget(ctx, dir, "refs/heads/"+target); !ok {
			// refs/for/<target>/<topic>
			if i := strings.LastIndex(rest, "/"); i > 0 {
				target = rest[:i]
				if topic == "" {
					topic = rest[i+1:]
				}
			}
		}
		tgtTip, ok := gitx.RefTarget(ctx, dir, "refs/heads/"+target)
		if !ok || !gitx.SafeRevision(target) {
			res.Reason = "target branch " + target + " does not exist"
			out = append(out, res)
			continue
		}
		if gitx.IsAncestor(ctx, dir, c.New, tgtTip) {
			res.Reason = "no new commits: everything is already in " + target
			out = append(out, res)
			continue
		}
		// Existing MR with this topic?
		var existing *MergeRequest
		if topic != "" {
			rs.mu.RLock()
			for _, m := range rs.mrs {
				if m.State == "opened" && m.SourceBranch == "" && m.Topic == topic && m.TargetBranch == target {
					existing = m
				}
			}
			rs.mu.RUnlock()
		}
		if existing != nil {
			rs.mu.RLock()
			author, n, prev := existing.AuthorID, existing.Number, existing.Latest().Head
			rs.mu.RUnlock()
			if u.ID != author && !rs.repo.CanWrite(u.ID) {
				res.Reason = fmt.Sprintf("merge request !%d belongs to someone else", n)
				out = append(out, res)
				continue
			}
			if err := s.addVersionLocked(ctx, rs, existing, c.New, u); err != nil {
				res.Reason = "could not record version: " + err.Error()
				out = append(out, res)
				continue
			}
			rs.mu.RLock()
			v := len(existing.Versions)
			rs.mu.RUnlock()
			res.OK, res.Number = true, n
			res.RefName = fmt.Sprintf("refs/merge-requests/%d/head", n)
			res.OldOID, res.NewOID = prev, c.New
			res.Forced = prev != "" && !gitx.IsAncestor(ctx, dir, prev, c.New)
			res.Message = fmt.Sprintf("Updated merge request !%d to version %d: %s/%s/-/merge_requests/%d", n, v, baseURL, rs.repo.Path(), n)
			out = append(out, res)
			continue
		}
		title := strings.TrimSpace(opts["title"])
		if title == "" {
			if cm, err := gitx.GetCommit(ctx, dir, c.New); err == nil {
				title = cm.Subject
			}
		}
		m, err := s.createMRLocked(ctx, rs, u, mrOpened{Title: cleanText(title, 255), Description: cleanText(opts["description"], 64*1024), Target: target, Topic: topic}, c.New)
		if err != nil {
			res.Reason = "could not create merge request: " + err.Error()
			out = append(out, res)
			continue
		}
		rs.mu.RLock()
		n, tp := m.Number, m.Topic
		rs.mu.RUnlock()
		res.OK, res.Number = true, n
		res.RefName = fmt.Sprintf("refs/merge-requests/%d/head", n)
		res.OldOID, res.NewOID = gitx.ZeroOID, c.New
		res.Message = fmt.Sprintf("Created merge request !%d: %s/%s/-/merge_requests/%d\nTo add a new version later: git push origin HEAD:refs/for/%s -o topic=%s", n, baseURL, rs.repo.Path(), n, target, tp)
		out = append(out, res)
	}
	s.MarkDirty(repoID)
	return out
}

// FileChange is one file edit for CommitFiles / ProposeChange.
type FileChange struct {
	Path    string `json:"path" jsonschema:"file path relative to the repository root"`
	Content string `json:"content,omitempty" jsonschema:"new file content (UTF-8)"`
	Delete  bool   `json:"delete,omitempty" jsonschema:"set true to delete the file"`
}

func validPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\x00") || strings.Contains(p, "\\") {
		return false
	}
	if path.Clean(p) != p {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.EqualFold(seg, ".git") {
			return false
		}
	}
	return true
}

// buildCommit creates a commit on top of base (may be "") applying files.
func buildCommit(ctx context.Context, dir, base string, files []FileChange, message string, id gitx.Identity) (string, error) {
	if len(files) == 0 {
		return "", fmt.Errorf("%w: no files", ErrInvalid)
	}
	if strings.TrimSpace(message) == "" {
		return "", fmt.Errorf("%w: commit message required", ErrInvalid)
	}
	tmp, err := os.CreateTemp("", "newgit-index-*")
	if err != nil {
		return "", err
	}
	idx := tmp.Name()
	tmp.Close()
	os.Remove(idx)
	defer os.Remove(idx)
	env := []string{"GIT_INDEX_FILE=" + idx}
	if base != "" {
		if _, err := gitx.Run(ctx, dir, &gitx.RunOpts{Env: env}, "read-tree", "--end-of-options", base); err != nil {
			return "", err
		}
	}
	for _, f := range files {
		if !validPath(f.Path) {
			return "", fmt.Errorf("%w: bad path %q", ErrInvalid, f.Path)
		}
		if f.Delete {
			if _, err := gitx.Run(ctx, dir, &gitx.RunOpts{Env: env}, "update-index", "--force-remove", "--", f.Path); err != nil {
				return "", err
			}
			continue
		}
		oid, err := gitx.HashObject(ctx, dir, []byte(f.Content), nil)
		if err != nil {
			return "", err
		}
		if _, err := gitx.Run(ctx, dir, &gitx.RunOpts{Env: env}, "update-index", "--add", "--cacheinfo", "100644,"+oid+","+f.Path); err != nil {
			return "", err
		}
	}
	tree, err := gitx.RunStringEnv(ctx, dir, env, "write-tree")
	if err != nil {
		return "", err
	}
	var parents []string
	if base != "" {
		if bt, err := gitx.RunString(ctx, dir, "rev-parse", base+"^{tree}"); err == nil && bt == tree {
			return "", fmt.Errorf("%w: the changes are identical to the current content", ErrInvalid)
		}
		parents = []string{base}
	}
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	return gitx.CommitTree(ctx, dir, tree, parents, message, id, nil)
}

// CommitFiles commits file changes straight to a branch (owner only).
// startFrom names the branch to start from when branch does not exist yet.
func (s *Store) CommitFiles(ctx context.Context, repoID string, u User, via, branch, startFrom, message string, files []FileChange) (string, error) {
	rs, err := s.repoState(repoID)
	if err != nil {
		return "", err
	}
	if !rs.repo.CanWrite(u.ID) {
		return "", fmt.Errorf("%w: only the owner can commit to branches; use propose_change instead", ErrForbidden)
	}
	if !gitx.SafeRevision(branch) || strings.HasPrefix(branch, "refs/") {
		return "", fmt.Errorf("%w: bad branch name", ErrInvalid)
	}
	dir := rs.repo.Dir
	if _, err := gitx.Run(ctx, dir, nil, "check-ref-format", "--branch", branch); err != nil {
		return "", fmt.Errorf("%w: bad branch name", ErrInvalid)
	}
	rs.pushMu.Lock()
	defer rs.pushMu.Unlock()
	ref := "refs/heads/" + branch
	old, exists := gitx.RefTarget(ctx, dir, ref)
	base := old
	if !exists {
		old = ""
		base = ""
		if startFrom != "" {
			b, ok := gitx.RefTarget(ctx, dir, "refs/heads/"+startFrom)
			if !ok {
				return "", fmt.Errorf("%w: start branch %q not found", ErrInvalid, startFrom)
			}
			base = b
		}
	}
	before := SnapshotRefs(ctx, dir)
	commit, err := buildCommit(ctx, dir, base, files, message, u.Identity())
	if err != nil {
		return "", err
	}
	if err := gitx.UpdateRef(ctx, dir, ref, commit, old, nil); err != nil {
		return "", fmt.Errorf("branch moved, try again: %w", err)
	}
	s.RecordPush(ctx, repoID, u, via, before, SnapshotRefs(ctx, dir))
	return commit, nil
}

// ProposeChange commits file changes on top of a target branch and opens a
// merge request for them. Anyone signed in may do this (same path as AGit).
func (s *Store) ProposeChange(ctx context.Context, repoID string, u User, via, target, title, description, message string, files []FileChange, baseURL string) (MergeRequest, error) {
	rs, err := s.repoState(repoID)
	if err != nil {
		return MergeRequest{}, err
	}
	dir := rs.repo.Dir
	tgt, ok := gitx.RefTarget(ctx, dir, "refs/heads/"+target)
	if !ok || !gitx.SafeRevision(target) {
		return MergeRequest{}, fmt.Errorf("%w: target branch %q not found", ErrInvalid, target)
	}
	rs.pushMu.Lock()
	defer rs.pushMu.Unlock()
	before := SnapshotRefs(ctx, dir)
	commit, err := buildCommit(ctx, dir, tgt, files, message, u.Identity())
	if err != nil {
		return MergeRequest{}, err
	}
	if strings.TrimSpace(title) == "" {
		title = strings.SplitN(message, "\n", 2)[0]
	}
	res := s.HandleAGit(ctx, repoID, u, []AGitCommand{{Old: gitx.ZeroOID, New: commit, Ref: "refs/for/" + target}}, map[string]string{"title": title, "description": description}, baseURL)
	if len(res) == 0 || !res[0].OK {
		reason := "unknown error"
		if len(res) > 0 {
			reason = res[0].Reason
		}
		return MergeRequest{}, fmt.Errorf("%w: %s", ErrInvalid, reason)
	}
	s.RecordPush(ctx, repoID, u, via, before, SnapshotRefs(ctx, dir))
	return s.MergeRequest(repoID, res[0].Number)
}
