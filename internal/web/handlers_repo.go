package web

import (
	"bytes"
	"net/http"
	"net/url"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/AlphaTechini/newgit/internal/gitx"
	"github.com/AlphaTechini/newgit/internal/store"
)

const maxBlobDisplay = 1 << 20 // 1 MiB

func isBinary(b []byte) bool {
	n := len(b)
	if n > 8000 {
		n = 8000
	}
	return bytes.IndexByte(b[:n], 0) >= 0 || !utf8.Valid(b[:n])
}

// imageTypes maps file extensions to the only image content types ever served
// inline. SVG is deliberately absent: it can carry script.
var imageTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp",
}

const maxRawImage = 50 << 20

// sniffImage returns the content type when both the extension and the
// content say this is a raster image we serve inline.
func sniffImage(name string, b []byte) (string, bool) {
	want, ok := imageTypes[strings.ToLower(path.Ext(name))]
	if !ok {
		return "", false
	}
	n := len(b)
	if n > 512 {
		n = 512
	}
	got := http.DetectContentType(b[:n])
	if got != want {
		return "", false
	}
	return got, true
}

func readmeOf(entries []gitx.TreeEntry) string {
	for _, cand := range []string{"readme.md", "readme", "readme.txt", "readme.markdown"} {
		for _, e := range entries {
			if e.Type == "blob" && strings.ToLower(e.Name) == cand {
				return e.Path
			}
		}
	}
	return ""
}

type treeData struct {
	Commit     string
	Path       string
	Entries    []gitx.TreeEntry
	LastCommit gitx.Commit
	Readme     string
	ReadmeHTML any
	Pusher     *store.Push
	Branches   []gitx.Branch
	IsRoot     bool
}

func (s *Server) treeData(r *http.Request, repo store.Repo, commit, p string) (*treeData, error) {
	ctx := r.Context()
	entries, err := gitx.ListTree(ctx, repo.Dir, commit, p)
	if err != nil {
		return nil, err
	}
	td := &treeData{Commit: commit, Path: p, Entries: entries, IsRoot: p == ""}
	if cs, err := gitx.Log(ctx, repo.Dir, commit, p, 0, 1); err == nil && len(cs) > 0 {
		td.LastCommit = cs[0]
		if push, ok := s.Store.PusherOf(repo.ID, cs[0].OID); ok {
			td.Pusher = &push
		}
	}
	if rp := readmeOf(entries); rp != "" {
		if b, err := gitx.CatBlob(ctx, repo.Dir, commit+":"+rp); err == nil && len(b) < maxBlobDisplay {
			td.Readme = rp
			if strings.HasSuffix(strings.ToLower(rp), ".md") || strings.HasSuffix(strings.ToLower(rp), ".markdown") {
				td.ReadmeHTML = renderMarkdownDoc(string(b))
			} else {
				td.ReadmeHTML = string(b)
			}
		}
	}
	td.Branches = repoRefs(r, repo.Dir).Branches
	return td, nil
}

func (s *Server) handleRepoHome(w http.ResponseWriter, r *http.Request, repo store.Repo) {
	rc := s.repoCtx(r, repo, "")
	page := &Page{Title: repo.Path(), Repo: rc, Nav: "overview"}
	if rc.Empty {
		s.render(w, r, "repo_empty.html", page)
		return
	}
	commit, ok := repoRefs(r, repo.Dir).Commit("refs/heads/" + rc.Default)
	if !ok {
		s.render(w, r, "repo_empty.html", page)
		return
	}
	td, err := s.treeData(r, repo, commit, "")
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	page.Data = map[string]any{"Tree": td, "Pushes": s.Store.RecentPushes(repo.ID, 5)}
	s.render(w, r, "repo_home.html", page)
}

func (s *Server) handleTree(w http.ResponseWriter, r *http.Request, repo store.Repo, rest string) {
	ref, commit, p, ok := resolveRefPath(r, repo.Dir, rest)
	if !ok {
		s.errorPage(w, r, 404, "Branch, tag or commit not found.")
		return
	}
	p, ok = cleanRepoPath(p)
	if !ok {
		s.errorPage(w, r, 400, "Bad path.")
		return
	}
	if p != "" {
		if t, _, err := gitx.ObjectInfo(r.Context(), repo.Dir, commit+":"+p); err != nil || t != "tree" {
			if err == nil && t == "blob" {
				http.Redirect(w, r, "/"+repo.Path()+"/-/blob/"+ref+"/"+p, http.StatusFound)
				return
			}
			s.errorPage(w, r, 404, "Path not found.")
			return
		}
	}
	td, err := s.treeData(r, repo, commit, p)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	rc := s.repoCtx(r, repo, ref)
	parent := path.Dir(p)
	if parent == "." {
		parent = ""
	}
	s.render(w, r, "tree.html", &Page{Title: p + " · " + repo.Path(), Repo: rc, Nav: "files", Data: map[string]any{"Tree": td, "Parent": parent}})
}

func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request, repo store.Repo, rest string, raw bool) {
	ref, commit, p, ok := resolveRefPath(r, repo.Dir, rest)
	if !ok || p == "" {
		s.errorPage(w, r, 404, "File not found.")
		return
	}
	p, ok = cleanRepoPath(p)
	if !ok {
		s.errorPage(w, r, 400, "Bad path.")
		return
	}
	ctx := r.Context()
	t, size, err := gitx.ObjectInfo(ctx, repo.Dir, commit+":"+p)
	if err != nil || t != "blob" {
		if err == nil && t == "tree" {
			http.Redirect(w, r, "/"+repo.Path()+"/-/tree/"+ref+"/"+p, http.StatusFound)
			return
		}
		s.errorPage(w, r, 404, "File not found.")
		return
	}
	if raw {
		if size > 50<<20 {
			http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
			return
		}
		b, err := gitx.CatBlob(ctx, repo.Dir, commit+":"+p)
		if err != nil {
			s.storeError(w, r, err)
			return
		}
		// Repository content can never run as HTML on our origin: it is served
		// as plain text, octet-stream, or (verified png/jpeg/gif/webp only) an
		// inline image. SVG is never served as an image.
		if ct, ok := sniffImage(p, b); ok {
			w.Header().Set("Content-Type", ct)
			w.Header().Set("Content-Disposition", "inline")
		} else if isBinary(b) {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(path.Base(p), `"`, "")+`"`)
		} else {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		_, _ = w.Write(b)
		return
	}
	data := map[string]any{"Path": p, "Commit": commit, "Size": size, "Name": path.Base(p), "IsImage": false, "RawURL": ""}
	isImage := false
	if _, ext := imageTypes[strings.ToLower(path.Ext(p))]; ext && size <= maxRawImage {
		// sniffImage looks at 512 bytes at most, so read no more than that.
		if b, err := gitx.CatBlobHead(ctx, repo.Dir, commit+":"+p, 512); err == nil {
			_, isImage = sniffImage(p, b)
		}
	}
	if isImage {
		u := url.URL{Path: "/" + repo.Path() + "/-/raw/" + ref + "/" + p}
		data["IsImage"], data["RawURL"] = true, u.EscapedPath()
	} else if size > maxBlobDisplay {
		data["TooLarge"] = true
	} else {
		b, err := gitx.CatBlob(ctx, repo.Dir, commit+":"+p)
		if err != nil {
			s.storeError(w, r, err)
			return
		}
		if isBinary(b) {
			data["Binary"] = true
		} else {
			lower := strings.ToLower(p)
			if strings.HasSuffix(lower, ".md") || strings.HasSuffix(lower, ".markdown") {
				data["Markdown"] = renderMarkdownDoc(string(b))
			}
			data["Highlighted"] = highlight(p, string(b))
			data["Lines"] = strings.Count(string(b), "\n")
		}
	}
	if cs, err := gitx.Log(ctx, repo.Dir, commit, p, 0, 1); err == nil && len(cs) > 0 {
		data["LastCommit"] = cs[0]
	}
	rc := s.repoCtx(r, repo, ref)
	s.render(w, r, "blob.html", &Page{Title: p + " · " + repo.Path(), Repo: rc, Nav: "files", Data: data})
}

const commitsPerPage = 40

func (s *Server) handleCommits(w http.ResponseWriter, r *http.Request, repo store.Repo, rest string) {
	rc := s.repoCtx(r, repo, "")
	if rest == "" {
		rest = rc.Default
	}
	ref, commit, p, ok := resolveRefPath(r, repo.Dir, rest)
	if !ok {
		if rc.Empty {
			s.render(w, r, "repo_empty.html", &Page{Title: repo.Path(), Repo: rc, Nav: "commits"})
			return
		}
		s.errorPage(w, r, 404, "Branch, tag or commit not found.")
		return
	}
	p, ok = cleanRepoPath(p)
	if !ok {
		s.errorPage(w, r, 400, "Bad path.")
		return
	}
	page := atoi(r.URL.Query().Get("page"), 1)
	if page < 1 {
		page = 1
	}
	commits, err := gitx.Log(r.Context(), repo.Dir, commit, p, (page-1)*commitsPerPage, commitsPerPage+1)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	more := len(commits) > commitsPerPage
	if more {
		commits = commits[:commitsPerPage]
	}
	type row struct {
		gitx.Commit
		Pusher *store.Push
	}
	rows := make([]row, 0, len(commits))
	for _, c := range commits {
		rw := row{Commit: c}
		if push, ok := s.Store.PusherOf(repo.ID, c.OID); ok {
			rw.Pusher = &push
		}
		rows = append(rows, rw)
	}
	rc = s.repoCtx(r, repo, ref)
	s.render(w, r, "commits.html", &Page{Title: "Commits · " + repo.Path(), Repo: rc, Nav: "commits", Data: map[string]any{
		"Commits": rows, "Path": p, "Page": page, "More": more,
	}})
}

func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request, repo store.Repo, sha string) {
	sha = strings.Trim(sha, "/")
	if !isHex(sha) || len(sha) < 4 {
		s.errorPage(w, r, 404, "Commit not found.")
		return
	}
	full, err := gitx.ResolveCommit(r.Context(), repo.Dir, sha)
	if err != nil {
		s.errorPage(w, r, 404, "Commit not found.")
		return
	}
	c, err := gitx.GetCommit(r.Context(), repo.Dir, full)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	diffs, err := gitx.CommitDiff(r.Context(), repo.Dir, c)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	data := map[string]any{"Commit": c, "Diffs": diffs}
	if push, ok := s.Store.PusherOf(repo.ID, c.OID); ok {
		data["Pusher"] = push
	}
	adds, dels := 0, 0
	for _, d := range diffs {
		adds += d.Additions
		dels += d.Deletions
	}
	data["Adds"], data["Dels"] = adds, dels
	s.render(w, r, "commit.html", &Page{Title: c.Subject + " · " + repo.Path(), Repo: s.repoCtx(r, repo, ""), Nav: "commits", Data: data})
}

func (s *Server) handleBranches(w http.ResponseWriter, r *http.Request, repo store.Repo) {
	rc := s.repoCtx(r, repo, "")
	refs := repoRefs(r, repo.Dir)
	bs := refs.Branches
	def, hasDef := refs.Commit("refs/heads/" + rc.Default)
	for i := range bs {
		if hasDef && bs[i].Name != rc.Default {
			bs[i].Ahead, bs[i].Behind, _ = gitx.AheadBehind(r.Context(), repo.Dir, bs[i].Commit.OID, def)
		}
	}
	mrBySource := map[string]int{}
	for _, m := range s.Store.MergeRequests(repo.ID, "opened") {
		if m.SourceBranch != "" {
			mrBySource[m.SourceBranch] = m.Number
		}
	}
	s.render(w, r, "branches.html", &Page{Title: "Branches · " + repo.Path(), Repo: rc, Nav: "branches", Data: map[string]any{"Branches": bs, "MRs": mrBySource}})
}

func (s *Server) handleTags(w http.ResponseWriter, r *http.Request, repo store.Repo) {
	rc := s.repoCtx(r, repo, "")
	tags := repoRefs(r, repo.Dir).Tags
	if tags == nil {
		tags = []gitx.TagInfo{}
	}
	s.render(w, r, "tags.html", &Page{Title: "Tags · " + repo.Path(), Repo: rc, Nav: "tags", Data: map[string]any{"Tags": tags}})
}

func (s *Server) handlePushes(w http.ResponseWriter, r *http.Request, repo store.Repo) {
	rc := s.repoCtx(r, repo, "")
	s.render(w, r, "pushes.html", &Page{Title: "Push log · " + repo.Path(), Repo: rc, Nav: "pushes", Data: map[string]any{"Pushes": s.Store.RecentPushes(repo.ID, 100)}})
}
