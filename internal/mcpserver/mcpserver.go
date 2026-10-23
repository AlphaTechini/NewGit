// Package mcpserver exposes NewGit to AI agents over the Model Context
// Protocol (streamable HTTP). Agents authenticate with a personal access
// token issued by a device, sent as a Bearer token. Permissions are the same
// as in the web UI.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AlphaTechini/newgit/internal/gitx"
	"github.com/AlphaTechini/newgit/internal/store"
)

type tools struct {
	st      *store.Store
	baseURL string
	version string
	list    []toolDef
	byName  map[string]toolDef
}

// call is the authenticated context of one tool call.
type call struct {
	user     store.User
	canWrite bool
}

func (t *tools) user(req *call) (store.User, bool, error) {
	return req.user, req.canWrite, nil
}

func (t *tools) writer(req *call) (store.User, error) {
	if !req.canWrite {
		return req.user, errors.New("this token is read-only; create a write token on the device page")
	}
	return req.user, nil
}

func (t *tools) repo(path string) (store.Repo, error) {
	owner, name, ok := strings.Cut(strings.Trim(path, "/"), "/")
	if !ok {
		return store.Repo{}, fmt.Errorf("repo must be \"owner/name\", got %q", path)
	}
	r, found := t.st.RepoByPath(owner, strings.TrimSuffix(name, ".git"))
	if !found {
		return store.Repo{}, fmt.Errorf("repository %s not found", path)
	}
	return r, nil
}

func (t *tools) resolve(ctx context.Context, r store.Repo, ref string) (string, string, error) {
	if ref == "" {
		ref = gitx.HeadBranch(ctx, r.Dir)
	}
	if gitx.SafeRevision(ref) {
		for _, full := range []string{"refs/heads/" + ref, "refs/tags/" + ref} {
			if c, ok := gitx.RefTarget(ctx, r.Dir, full+"^{commit}"); ok {
				return ref, c, nil
			}
		}
	}
	c, err := gitx.ResolveCommit(ctx, r.Dir, ref)
	if err != nil {
		return "", "", fmt.Errorf("ref %q not found (is the repository empty?)", ref)
	}
	return ref, c, nil
}

// ---------- inputs / outputs ----------

type empty struct{}

type repoIn struct {
	Repo string `json:"repo" jsonschema:"repository as owner/name"`
}

type createRepoIn struct {
	Name        string `json:"name" jsonschema:"repository name (letters, digits, . _ -)"`
	Description string `json:"description,omitempty" jsonschema:"short description"`
	InitReadme  bool   `json:"init_readme,omitempty" jsonschema:"create an initial commit with a README on main"`
}

type treeIn struct {
	Repo string `json:"repo" jsonschema:"repository as owner/name"`
	Ref  string `json:"ref,omitempty" jsonschema:"branch, tag or commit (default: default branch)"`
	Path string `json:"path,omitempty" jsonschema:"directory path, empty for root"`
}

type fileIn struct {
	Repo string `json:"repo" jsonschema:"repository as owner/name"`
	Ref  string `json:"ref,omitempty" jsonschema:"branch, tag or commit (default: default branch)"`
	Path string `json:"path" jsonschema:"file path"`
}

type commitsIn struct {
	Repo  string `json:"repo" jsonschema:"repository as owner/name"`
	Ref   string `json:"ref,omitempty" jsonschema:"branch, tag or commit (default: default branch)"`
	Path  string `json:"path,omitempty" jsonschema:"only commits touching this path"`
	Limit int    `json:"limit,omitempty" jsonschema:"max commits (default 20, max 100)"`
	Skip  int    `json:"skip,omitempty" jsonschema:"commits to skip for paging"`
}

type commitIn struct {
	Repo string `json:"repo" jsonschema:"repository as owner/name"`
	SHA  string `json:"sha" jsonschema:"commit id (full or abbreviated)"`
}

type listIn struct {
	Repo  string `json:"repo" jsonschema:"repository as owner/name"`
	State string `json:"state,omitempty" jsonschema:"opened (default), closed, merged (MRs only) or all"`
}

type numIn struct {
	Repo   string `json:"repo" jsonschema:"repository as owner/name"`
	Number int    `json:"number" jsonschema:"issue or merge request number"`
}

type createIssueIn struct {
	Repo  string `json:"repo" jsonschema:"repository as owner/name"`
	Title string `json:"title" jsonschema:"issue title"`
	Body  string `json:"body,omitempty" jsonschema:"Markdown description"`
}

type commentIn struct {
	Repo   string `json:"repo" jsonschema:"repository as owner/name"`
	Number int    `json:"number" jsonschema:"issue or merge request number"`
	Body   string `json:"body" jsonschema:"Markdown comment"`
}

type stateIn struct {
	Repo   string `json:"repo" jsonschema:"repository as owner/name"`
	Number int    `json:"number" jsonschema:"issue or merge request number"`
	State  string `json:"state" jsonschema:"opened or closed"`
}

type createMRIn struct {
	Repo        string `json:"repo" jsonschema:"repository as owner/name"`
	Source      string `json:"source_branch" jsonschema:"branch with the changes"`
	Target      string `json:"target_branch,omitempty" jsonschema:"branch to merge into (default: default branch)"`
	Title       string `json:"title,omitempty" jsonschema:"title (default: latest commit subject)"`
	Description string `json:"description,omitempty" jsonschema:"Markdown description"`
}

type mrDiffIn struct {
	Repo    string `json:"repo" jsonschema:"repository as owner/name"`
	Number  int    `json:"number" jsonschema:"merge request number"`
	Version int    `json:"version,omitempty" jsonschema:"version number (default: latest)"`
}

type rangeIn struct {
	Repo   string `json:"repo" jsonschema:"repository as owner/name"`
	Number int    `json:"number" jsonschema:"merge request number"`
	From   int    `json:"from,omitempty" jsonschema:"older version (default: latest-1)"`
	To     int    `json:"to,omitempty" jsonschema:"newer version (default: latest)"`
}

type commitFilesIn struct {
	Repo      string             `json:"repo" jsonschema:"repository as owner/name"`
	Branch    string             `json:"branch" jsonschema:"branch to commit to (created if missing)"`
	StartFrom string             `json:"start_from,omitempty" jsonschema:"when creating a new branch, branch to start from (default: default branch)"`
	Message   string             `json:"message" jsonschema:"commit message"`
	Files     []store.FileChange `json:"files" jsonschema:"files to write or delete"`
}

type proposeIn struct {
	Repo        string             `json:"repo" jsonschema:"repository as owner/name"`
	Target      string             `json:"target_branch,omitempty" jsonschema:"branch the change is for (default: default branch)"`
	Title       string             `json:"title,omitempty" jsonschema:"merge request title (default: first line of message)"`
	Description string             `json:"description,omitempty" jsonschema:"merge request description"`
	Message     string             `json:"message" jsonschema:"commit message"`
	Files       []store.FileChange `json:"files" jsonschema:"files to write or delete"`
}

const instructions = "NewGit is a GitLab-style Git host. Repositories are addressed as owner/name. Issues and merge requests are numbered per repository. " +
	"Use propose_change to suggest code changes as a merge request (works without write access to branches); use commit_files only on repositories you own. " +
	"Merge requests keep every pushed version; use get_merge_request_range_diff to see what changed between versions."

// registerTools adds every tool to t.
func registerTools(t *tools) {
	st := t.st

	add(t, "whoami", "Show the account this token belongs to.", func(ctx context.Context, req *call, _ empty) (string, error) {
		u, w, err := t.user(req)
		if err != nil {
			return "", err
		}
		scope := "read"
		if w {
			scope = "write"
		}
		return fmt.Sprintf("@%s (account %s), token scope: %s, profile: %s/%s", u.Username, u.ID, scope, t.baseURL, u.Username), nil
	})

	add(t, "list_repos", "List all repositories on this NewGit server.", func(ctx context.Context, req *call, _ empty) (string, error) {
		var b strings.Builder
		for _, r := range st.ListRepos("") {
			oi, om := st.CountOpen(r.ID)
			fmt.Fprintf(&b, "%s  (%d open issues, %d open MRs)  %s\n", r.Path(), oi, om, r.Description)
		}
		if b.Len() == 0 {
			return "No repositories yet.", nil
		}
		return b.String(), nil
	})

	add(t, "create_repo", "Create a repository owned by the token's account.", func(ctx context.Context, req *call, in createRepoIn) (string, error) {
		u, err := t.writer(req)
		if err != nil {
			return "", err
		}
		r, err := st.CreateRepo(ctx, u, in.Name, in.Description)
		if err != nil {
			return "", err
		}
		if in.InitReadme {
			_, _ = st.CommitFiles(ctx, r.ID, u, "mcp", "main", "", "Initial commit", []store.FileChange{{Path: "README.md", Content: "# " + r.Name + "\n\n" + in.Description + "\n"}})
		}
		return fmt.Sprintf("Created %s\nWeb: %s/%s\nClone: %s/%s.git", r.Path(), t.baseURL, r.Path(), t.baseURL, r.Path()), nil
	})

	add(t, "list_branches", "List branches with their latest commit.", func(ctx context.Context, req *call, in repoIn) (string, error) {
		r, err := t.repo(in.Repo)
		if err != nil {
			return "", err
		}
		bs, err := gitx.ListBranches(ctx, r.Dir)
		if err != nil {
			return "", err
		}
		def := gitx.HeadBranch(ctx, r.Dir)
		var b strings.Builder
		for _, br := range bs {
			mark := ""
			if br.Name == def {
				mark = " (default)"
			}
			fmt.Fprintf(&b, "%s%s  %s %s\n", br.Name, mark, short(br.Commit.OID), br.Commit.Subject)
		}
		if b.Len() == 0 {
			return "No branches (empty repository).", nil
		}
		return b.String(), nil
	})

	add(t, "list_tree", "List files and directories at a path.", func(ctx context.Context, req *call, in treeIn) (string, error) {
		r, err := t.repo(in.Repo)
		if err != nil {
			return "", err
		}
		_, c, err := t.resolve(ctx, r, in.Ref)
		if err != nil {
			return "", err
		}
		entries, err := gitx.ListTree(ctx, r.Dir, c, strings.Trim(in.Path, "/"))
		if err != nil {
			return "", fmt.Errorf("path not found")
		}
		var b strings.Builder
		for _, e := range entries {
			if e.Type == "tree" {
				fmt.Fprintf(&b, "%s/\n", e.Path)
			} else {
				fmt.Fprintf(&b, "%s  (%d bytes)\n", e.Path, e.Size)
			}
		}
		return b.String(), nil
	})

	add(t, "read_file", "Read a text file's content.", func(ctx context.Context, req *call, in fileIn) (string, error) {
		r, err := t.repo(in.Repo)
		if err != nil {
			return "", err
		}
		_, c, err := t.resolve(ctx, r, in.Ref)
		if err != nil {
			return "", err
		}
		p := strings.Trim(in.Path, "/")
		size, err := gitx.ObjectSize(ctx, r.Dir, c+":"+p)
		if err != nil {
			return "", fmt.Errorf("file not found: %s", p)
		}
		if size > 512*1024 {
			return "", fmt.Errorf("file is %d bytes; too large to return", size)
		}
		b, err := gitx.CatBlob(ctx, r.Dir, c+":"+p)
		if err != nil {
			return "", fmt.Errorf("not a file: %s", p)
		}
		if !utf8.Valid(b) {
			return fmt.Sprintf("%s is a binary file (%d bytes).", p, len(b)), nil
		}
		return string(b), nil
	})

	add(t, "list_commits", "List commits on a ref (newest first), with the verified pusher of each commit.", func(ctx context.Context, req *call, in commitsIn) (string, error) {
		r, err := t.repo(in.Repo)
		if err != nil {
			return "", err
		}
		_, c, err := t.resolve(ctx, r, in.Ref)
		if err != nil {
			return "", err
		}
		limit := in.Limit
		if limit <= 0 || limit > 100 {
			limit = 20
		}
		cs, err := gitx.Log(ctx, r.Dir, c, strings.Trim(in.Path, "/"), max(in.Skip, 0), limit)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		for _, cm := range cs {
			pusher := "unknown"
			if p, ok := st.PusherOf(r.ID, cm.OID); ok {
				pusher = "@" + p.By + " via " + p.Via
			}
			fmt.Fprintf(&b, "%s  %s  author: %s <%s>  %s  pushed by: %s\n", short(cm.OID), cm.Subject, cm.AuthorName, cm.AuthorEmail, cm.AuthorDate.UTC().Format(time.RFC3339), pusher)
		}
		return b.String(), nil
	})

	add(t, "get_commit", "Show a commit's metadata, verified pusher and diff.", func(ctx context.Context, req *call, in commitIn) (string, error) {
		r, err := t.repo(in.Repo)
		if err != nil {
			return "", err
		}
		if !gitx.SafeRevision(in.SHA) {
			return "", errors.New("bad sha")
		}
		full, err := gitx.ResolveCommit(ctx, r.Dir, in.SHA)
		if err != nil {
			return "", errors.New("commit not found")
		}
		cm, err := gitx.GetCommit(ctx, r.Dir, full)
		if err != nil {
			return "", err
		}
		pusher := "unknown"
		if p, ok := st.PusherOf(r.ID, cm.OID); ok {
			pusher = fmt.Sprintf("@%s via %s at %s", p.By, p.Via, p.At.UTC().Format(time.RFC3339))
		}
		diff := diffText(ctx, r.Dir, cm)
		return fmt.Sprintf("commit %s\nAuthor: %s <%s> %s\nCommitter: %s <%s>\nPushed by (verified): %s\nParents: %s\n\n%s\n\n%s\n%s",
			cm.OID, cm.AuthorName, cm.AuthorEmail, cm.AuthorDate.UTC().Format(time.RFC3339), cm.CommitterName, cm.CommitterEmail, pusher,
			strings.Join(cm.Parents, " "), cm.Subject, cm.Body, diff), nil
	})

	add(t, "list_issues", "List issues.", func(ctx context.Context, req *call, in listIn) (string, error) {
		r, err := t.repo(in.Repo)
		if err != nil {
			return "", err
		}
		st2 := in.State
		if st2 == "" {
			st2 = "opened"
		}
		var b strings.Builder
		for _, is := range st.Issues(r.ID, st2) {
			fmt.Fprintf(&b, "#%d [%s] %s  (by @%s, %d comments)\n", is.Number, is.State, is.Title, is.Author, len(is.Comments))
		}
		if b.Len() == 0 {
			return "No issues.", nil
		}
		return b.String(), nil
	})

	add(t, "get_issue", "Show an issue with its comments.", func(ctx context.Context, req *call, in numIn) (string, error) {
		r, err := t.repo(in.Repo)
		if err != nil {
			return "", err
		}
		is, err := st.Issue(r.ID, in.Number)
		if err != nil {
			return "", fmt.Errorf("issue #%d not found", in.Number)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "#%d %s\nState: %s\nAuthor: @%s\nCreated: %s\nURL: %s/%s/-/issues/%d\n\n%s\n", is.Number, is.Title, is.State, is.Author, is.Created.UTC().Format(time.RFC3339), t.baseURL, r.Path(), is.Number, is.Body)
		for _, e := range is.Timeline {
			if e.Comment != nil {
				fmt.Fprintf(&b, "\n--- @%s at %s:\n%s\n", e.Actor, e.At.UTC().Format(time.RFC3339), e.Comment.Body)
			} else {
				fmt.Fprintf(&b, "\n--- @%s %s this issue at %s\n", e.Actor, e.Kind, e.At.UTC().Format(time.RFC3339))
			}
		}
		return b.String(), nil
	})

	add(t, "create_issue", "Open an issue.", func(ctx context.Context, req *call, in createIssueIn) (string, error) {
		u, err := t.writer(req)
		if err != nil {
			return "", err
		}
		r, err := t.repo(in.Repo)
		if err != nil {
			return "", err
		}
		is, err := st.CreateIssue(ctx, r.ID, u, in.Title, in.Body)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Opened issue #%d: %s/%s/-/issues/%d", is.Number, t.baseURL, r.Path(), is.Number), nil
	})

	add(t, "comment_issue", "Comment on an issue.", func(ctx context.Context, req *call, in commentIn) (string, error) {
		u, err := t.writer(req)
		if err != nil {
			return "", err
		}
		r, err := t.repo(in.Repo)
		if err != nil {
			return "", err
		}
		if _, err := st.CommentIssue(ctx, r.ID, in.Number, u, in.Body); err != nil {
			return "", err
		}
		return fmt.Sprintf("Commented on issue #%d.", in.Number), nil
	})

	add(t, "set_issue_state", "Close or reopen an issue (issue author or repository owner).", func(ctx context.Context, req *call, in stateIn) (string, error) {
		u, err := t.writer(req)
		if err != nil {
			return "", err
		}
		r, err := t.repo(in.Repo)
		if err != nil {
			return "", err
		}
		if err := st.SetIssueState(ctx, r.ID, in.Number, u, in.State); err != nil {
			return "", err
		}
		return fmt.Sprintf("Issue #%d is now %s.", in.Number, in.State), nil
	})

	add(t, "list_merge_requests", "List merge requests.", func(ctx context.Context, req *call, in listIn) (string, error) {
		r, err := t.repo(in.Repo)
		if err != nil {
			return "", err
		}
		st2 := in.State
		if st2 == "" {
			st2 = "opened"
		}
		var b strings.Builder
		for _, m := range st.MergeRequests(r.ID, st2) {
			src := m.SourceBranch
			if src == "" {
				src = "refs/for push (topic " + m.Topic + ")"
			}
			fmt.Fprintf(&b, "!%d [%s] %s  %s -> %s  v%d  (by @%s)\n", m.Number, m.State, m.Title, src, m.TargetBranch, len(m.Versions), m.Author)
		}
		if b.Len() == 0 {
			return "No merge requests.", nil
		}
		return b.String(), nil
	})

	add(t, "get_merge_request", "Show a merge request: description, versions, merge status, commits and discussion.", func(ctx context.Context, req *call, in numIn) (string, error) {
		r, err := t.repo(in.Repo)
		if err != nil {
			return "", err
		}
		m, err := st.MergeRequest(r.ID, in.Number)
		if err != nil {
			return "", fmt.Errorf("merge request !%d not found", in.Number)
		}
		var b strings.Builder
		src := m.SourceBranch
		if src == "" {
			src = "refs/for push, topic " + m.Topic
		}
		fmt.Fprintf(&b, "!%d %s\nState: %s\nAuthor: @%s\nSource: %s\nTarget: %s\nURL: %s/%s/-/merge_requests/%d\nHead ref: refs/merge-requests/%d/head\n\n%s\n\nVersions:\n",
			m.Number, m.Title, m.State, m.Author, src, m.TargetBranch, t.baseURL, r.Path(), m.Number, m.Number, m.Description)
		for _, v := range m.Versions {
			fmt.Fprintf(&b, "  v%d %s pushed by @%s at %s (base %s)\n", v.N, short(v.Head), v.Pusher, v.At.UTC().Format(time.RFC3339), short(v.Base))
		}
		if m.State == "opened" {
			ms := st.CheckMerge(ctx, r, m)
			switch {
			case ms.Mergeable:
				b.WriteString("\nMerge status: can be merged without conflicts\n")
			case len(ms.Conflicts) > 0:
				fmt.Fprintf(&b, "\nMerge status: CONFLICTS in %s\n", strings.Join(ms.Conflicts, ", "))
			default:
				fmt.Fprintf(&b, "\nMerge status: %s\n", ms.Reason)
			}
		} else if m.State == "merged" {
			fmt.Fprintf(&b, "\nMerged by @%s as %s\n", m.MergedBy, m.MergeCommit)
		}
		if tip, ok := gitx.RefTarget(ctx, r.Dir, "refs/heads/"+m.TargetBranch); ok {
			if mb, err := gitx.MergeBase(ctx, r.Dir, tip, m.Latest().Head); err == nil {
				if cs, err := gitx.LogRange(ctx, r.Dir, m.Latest().Head, "^"+mb); err == nil {
					b.WriteString("\nCommits:\n")
					for _, c := range cs {
						fmt.Fprintf(&b, "  %s %s (%s)\n", short(c.OID), c.Subject, c.AuthorName)
					}
				}
			}
		}
		b.WriteString("\nDiscussion:\n")
		for _, e := range m.Timeline {
			switch {
			case e.Comment != nil:
				fmt.Fprintf(&b, "--- @%s on v%d at %s:\n%s\n", e.Actor, e.Comment.Version, e.At.UTC().Format(time.RFC3339), e.Comment.Body)
			case e.Kind == "version":
				fmt.Fprintf(&b, "--- @%s pushed v%d at %s\n", e.Actor, e.Version, e.At.UTC().Format(time.RFC3339))
			default:
				fmt.Fprintf(&b, "--- @%s %s at %s\n", e.Actor, e.Kind, e.At.UTC().Format(time.RFC3339))
			}
		}
		return b.String(), nil
	})

	add(t, "get_merge_request_diff", "Unified diff of a merge request version against its merge base.", func(ctx context.Context, req *call, in mrDiffIn) (string, error) {
		r, err := t.repo(in.Repo)
		if err != nil {
			return "", err
		}
		m, err := st.MergeRequest(r.ID, in.Number)
		if err != nil {
			return "", fmt.Errorf("merge request !%d not found", in.Number)
		}
		v := in.Version
		if v <= 0 || v > len(m.Versions) {
			v = len(m.Versions)
		}
		ver := m.Versions[v-1]
		base := ver.Base
		if tip, ok := gitx.RefTarget(ctx, r.Dir, "refs/heads/"+m.TargetBranch); ok && v == len(m.Versions) {
			if mb, err := gitx.MergeBase(ctx, r.Dir, tip, ver.Head); err == nil {
				base = mb
			}
		}
		if base == "" {
			return "", errors.New("no merge base with the target branch")
		}
		out, err := gitx.Run(ctx, r.Dir, nil, "diff", "--no-color", "--no-ext-diff", "-M", "--end-of-options", base, ver.Head)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Diff of !%d v%d (%s..%s):\n%s", m.Number, v, short(base), short(ver.Head), limit(string(out))), nil
	})

	add(t, "get_merge_request_range_diff", "git range-diff between two versions of a merge request: shows which commits are unchanged (=), changed (!), removed (<) or added (>), with the diff of the diffs.", func(ctx context.Context, req *call, in rangeIn) (string, error) {
		r, err := t.repo(in.Repo)
		if err != nil {
			return "", err
		}
		m, err := st.MergeRequest(r.ID, in.Number)
		if err != nil {
			return "", fmt.Errorf("merge request !%d not found", in.Number)
		}
		if len(m.Versions) < 2 {
			return "This merge request has only one version.", nil
		}
		to := in.To
		if to <= 0 || to > len(m.Versions) {
			to = len(m.Versions)
		}
		from := in.From
		if from <= 0 || from > len(m.Versions) {
			from = to - 1
		}
		if from == to {
			return "", errors.New("from and to must differ")
		}
		a, bv := m.Versions[from-1], m.Versions[to-1]
		out, err := gitx.RangeDiff(ctx, r.Dir, a.Base, a.Head, bv.Base, bv.Head)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("range-diff !%d v%d -> v%d:\n%s", m.Number, from, to, limit(out)), nil
	})

	add(t, "create_merge_request", "Open a merge request from an existing branch (repository owner only; others use propose_change).", func(ctx context.Context, req *call, in createMRIn) (string, error) {
		u, err := t.writer(req)
		if err != nil {
			return "", err
		}
		r, err := t.repo(in.Repo)
		if err != nil {
			return "", err
		}
		target := in.Target
		if target == "" {
			target = gitx.HeadBranch(ctx, r.Dir)
		}
		m, err := st.CreateMergeRequest(ctx, r.ID, u, in.Title, in.Description, in.Source, target)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Opened merge request !%d: %s/%s/-/merge_requests/%d", m.Number, t.baseURL, r.Path(), m.Number), nil
	})

	add(t, "comment_merge_request", "Comment on a merge request (the comment is tagged with the current version).", func(ctx context.Context, req *call, in commentIn) (string, error) {
		u, err := t.writer(req)
		if err != nil {
			return "", err
		}
		r, err := t.repo(in.Repo)
		if err != nil {
			return "", err
		}
		c, err := st.CommentMergeRequest(ctx, r.ID, in.Number, u, in.Body)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Commented on !%d (version %d).", in.Number, c.Version), nil
	})

	add(t, "set_merge_request_state", "Close or reopen a merge request (author or repository owner).", func(ctx context.Context, req *call, in stateIn) (string, error) {
		u, err := t.writer(req)
		if err != nil {
			return "", err
		}
		r, err := t.repo(in.Repo)
		if err != nil {
			return "", err
		}
		if err := st.SetMergeRequestState(ctx, r.ID, in.Number, u, in.State); err != nil {
			return "", err
		}
		return fmt.Sprintf("Merge request !%d is now %s.", in.Number, in.State), nil
	})

	add(t, "merge_merge_request", "Merge a merge request with a merge commit (repository owner only). Fails on conflicts.", func(ctx context.Context, req *call, in numIn) (string, error) {
		u, err := t.writer(req)
		if err != nil {
			return "", err
		}
		r, err := t.repo(in.Repo)
		if err != nil {
			return "", err
		}
		c, err := st.Merge(ctx, r.ID, in.Number, u, "mcp")
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Merged !%d as %s.", in.Number, c), nil
	})

	add(t, "commit_files", "Commit file changes directly to a branch of a repository you own.", func(ctx context.Context, req *call, in commitFilesIn) (string, error) {
		u, err := t.writer(req)
		if err != nil {
			return "", err
		}
		r, err := t.repo(in.Repo)
		if err != nil {
			return "", err
		}
		start := in.StartFrom
		if start == "" {
			if _, ok := gitx.RefTarget(ctx, r.Dir, "refs/heads/"+gitx.HeadBranch(ctx, r.Dir)); ok {
				start = gitx.HeadBranch(ctx, r.Dir)
			}
		}
		c, err := st.CommitFiles(ctx, r.ID, u, "mcp", in.Branch, start, in.Message, in.Files)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Committed %s to %s: %s/%s/-/commit/%s", short(c), in.Branch, t.baseURL, r.Path(), c), nil
	})

	add(t, "propose_change", "Commit file changes on top of a branch and open a merge request for them. Works for any signed-in account (same path as `git push origin HEAD:refs/for/<branch>`).", func(ctx context.Context, req *call, in proposeIn) (string, error) {
		u, err := t.writer(req)
		if err != nil {
			return "", err
		}
		r, err := t.repo(in.Repo)
		if err != nil {
			return "", err
		}
		target := in.Target
		if target == "" {
			target = gitx.HeadBranch(ctx, r.Dir)
		}
		m, err := st.ProposeChange(ctx, r.ID, u, "mcp", target, in.Title, in.Description, in.Message, in.Files, t.baseURL)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Opened merge request !%d (%s): %s/%s/-/merge_requests/%d", m.Number, short(m.Latest().Head), t.baseURL, r.Path(), m.Number), nil
	})
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

const maxText = 200 * 1024

func limit(s string) string {
	if len(s) > maxText {
		return s[:maxText] + "\n... (truncated)"
	}
	return s
}

func diffText(ctx context.Context, dir string, c gitx.Commit) string {
	var out []byte
	var err error
	if len(c.Parents) == 0 {
		out, err = gitx.Run(ctx, dir, nil, "diff-tree", "-p", "--no-color", "--root", "-r", "-M", "--end-of-options", c.OID)
	} else {
		out, err = gitx.Run(ctx, dir, nil, "diff", "--no-color", "--no-ext-diff", "-M", "--end-of-options", c.Parents[0], c.OID)
	}
	if err != nil {
		return "(diff unavailable)"
	}
	return limit(string(out))
}
