package web

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AlphaTechini/newgit/internal/gitx"
	"github.com/AlphaTechini/newgit/internal/store"
)

func stateFilter(r *http.Request) string {
	st := r.URL.Query().Get("state")
	switch st {
	case "closed", "merged", "all":
		return st
	}
	return "opened"
}

// ---------- issues ----------

func (s *Server) handleIssues(w http.ResponseWriter, r *http.Request, repo store.Repo, sub string) {
	sub = strings.Trim(sub, "/")
	rc := s.repoCtx(r, repo, "")
	switch {
	case sub == "":
		st := stateFilter(r)
		s.render(w, r, "issues.html", &Page{Title: "Issues · " + repo.Path(), Repo: rc, Nav: "issues", Data: map[string]any{
			"Issues": s.Store.Issues(repo.ID, st), "State": st,
			"CountOpen": len(s.Store.Issues(repo.ID, "opened")), "CountClosed": len(s.Store.Issues(repo.ID, "closed")),
		}})
	case sub == "new":
		u := s.requireUser(w, r)
		if u == nil {
			return
		}
		if r.Method == http.MethodGet {
			s.render(w, r, "issue_new.html", &Page{Title: "New issue · " + repo.Path(), Repo: rc, Nav: "issues"})
			return
		}
		if s.checkPost(w, r) == nil {
			return
		}
		is, err := s.Store.CreateIssue(r.Context(), repo.ID, *u, r.PostForm.Get("title"), r.PostForm.Get("body"))
		if err != nil {
			s.render(w, r, "issue_new.html", &Page{Title: "New issue", Repo: rc, Nav: "issues", Error: err.Error(), Data: map[string]any{"Title": r.PostForm.Get("title"), "Body": r.PostForm.Get("body")}})
			return
		}
		http.Redirect(w, r, withFlash(fmt.Sprintf("/%s/-/issues/%d", repo.Path(), is.Number), "issue_opened"), http.StatusSeeOther)
	default:
		numS, action, _ := strings.Cut(sub, "/")
		n, err := strconv.Atoi(numS)
		if err != nil {
			s.errorPage(w, r, 404, "Issue not found.")
			return
		}
		switch action {
		case "":
			is, err := s.Store.Issue(repo.ID, n)
			if err != nil {
				s.storeError(w, r, err)
				return
			}
			canState := false
			if u := currentUser(r); u != nil {
				canState = u.ID == is.AuthorID || repo.CanWrite(u.ID)
			}
			participants := []string{is.Author}
			for _, c := range is.Comments {
				participants = appendUnique(participants, c.Author)
			}
			s.render(w, r, "issue.html", &Page{Title: is.Title + " · Issue #" + numS, Repo: rc, Nav: "issues", Data: map[string]any{"Issue": is, "CanState": canState, "Participants": participants}})
		case "comment":
			u := s.checkPost(w, r)
			if u == nil {
				return
			}
			if _, err := s.Store.CommentIssue(r.Context(), repo.ID, n, *u, r.PostForm.Get("body")); err != nil {
				s.storeError(w, r, err)
				return
			}
			code := "commented"
			if r.PostForm.Get("close") == "1" {
				if s.Store.SetIssueState(r.Context(), repo.ID, n, *u, "closed") == nil {
					code = "closed"
				}
			}
			http.Redirect(w, r, withFlash(fmt.Sprintf("/%s/-/issues/%d#latest", repo.Path(), n), code), http.StatusSeeOther)
		case "state":
			u := s.checkPost(w, r)
			if u == nil {
				return
			}
			if err := s.Store.SetIssueState(r.Context(), repo.ID, n, *u, r.PostForm.Get("state")); err != nil {
				s.storeError(w, r, err)
				return
			}
			http.Redirect(w, r, withFlash(fmt.Sprintf("/%s/-/issues/%d", repo.Path(), n), stateFlash(r.PostForm.Get("state"))), http.StatusSeeOther)
		default:
			s.errorPage(w, r, 404, "Page not found.")
		}
	}
}

// ---------- merge requests ----------

func (s *Server) handleMRs(w http.ResponseWriter, r *http.Request, repo store.Repo, sub string) {
	sub = strings.Trim(sub, "/")
	rc := s.repoCtx(r, repo, "")
	switch {
	case sub == "":
		st := stateFilter(r)
		s.render(w, r, "mrs.html", &Page{Title: "Merge requests · " + repo.Path(), Repo: rc, Nav: "mrs", Data: map[string]any{
			"MRs": s.Store.MergeRequests(repo.ID, st), "State": st,
			"CountOpen":   len(s.Store.MergeRequests(repo.ID, "opened")),
			"CountMerged": len(s.Store.MergeRequests(repo.ID, "merged")),
			"CountClosed": len(s.Store.MergeRequests(repo.ID, "closed")),
		}})
	case sub == "new":
		s.handleNewMR(w, r, repo, rc)
	default:
		numS, action, _ := strings.Cut(sub, "/")
		n, err := strconv.Atoi(numS)
		if err != nil {
			s.errorPage(w, r, 404, "Merge request not found.")
			return
		}
		switch action {
		case "":
			s.handleMR(w, r, repo, rc, n)
		case "comment":
			u := s.checkPost(w, r)
			if u == nil {
				return
			}
			if _, err := s.Store.CommentMergeRequest(r.Context(), repo.ID, n, *u, r.PostForm.Get("body")); err != nil {
				s.storeError(w, r, err)
				return
			}
			http.Redirect(w, r, withFlash(fmt.Sprintf("/%s/-/merge_requests/%d#latest", repo.Path(), n), "commented"), http.StatusSeeOther)
		case "state":
			u := s.checkPost(w, r)
			if u == nil {
				return
			}
			if err := s.Store.SetMergeRequestState(r.Context(), repo.ID, n, *u, r.PostForm.Get("state")); err != nil {
				s.storeError(w, r, err)
				return
			}
			http.Redirect(w, r, withFlash(fmt.Sprintf("/%s/-/merge_requests/%d", repo.Path(), n), stateFlash(r.PostForm.Get("state"))), http.StatusSeeOther)
		case "merge":
			u := s.checkPost(w, r)
			if u == nil {
				return
			}
			if _, err := s.Store.Merge(r.Context(), repo.ID, n, *u, "web"); err != nil {
				s.storeError(w, r, err)
				return
			}
			http.Redirect(w, r, fmt.Sprintf("/%s/-/merge_requests/%d?flash=merged", repo.Path(), n), http.StatusSeeOther)
		default:
			s.errorPage(w, r, 404, "Page not found.")
		}
	}
}

func (s *Server) handleNewMR(w http.ResponseWriter, r *http.Request, repo store.Repo, rc *RepoCtx) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	branches, _ := gitx.ListBranches(r.Context(), repo.Dir)
	data := map[string]any{"Branches": branches, "Source": r.URL.Query().Get("source"), "Target": rc.Default, "Title": "", "Description": "", "Compared": false}
	if r.Method == http.MethodGet {
		s.compareBranches(r, repo, rc, data)
		s.render(w, r, "mr_new.html", &Page{Title: "New merge request", Repo: rc, Nav: "mrs", Data: data})
		return
	}
	if s.checkPost(w, r) == nil {
		return
	}
	src, tgt := r.PostForm.Get("source"), r.PostForm.Get("target")
	m, err := s.Store.CreateMergeRequest(r.Context(), repo.ID, *u, r.PostForm.Get("title"), r.PostForm.Get("description"), src, tgt)
	if err != nil {
		data["Source"], data["Target"], data["Title"], data["Description"] = src, tgt, r.PostForm.Get("title"), r.PostForm.Get("description")
		s.render(w, r, "mr_new.html", &Page{Title: "New merge request", Repo: rc, Nav: "mrs", Error: err.Error(), Data: data})
		return
	}
	http.Redirect(w, r, withFlash(fmt.Sprintf("/%s/-/merge_requests/%d", repo.Path(), m.Number), "mr_opened"), http.StatusSeeOther)
}

// stateFlash maps a requested issue/MR state to a flash code.
func stateFlash(state string) string {
	if state == "closed" {
		return "closed"
	}
	return "reopened"
}

func appendUnique(list []string, v string) []string {
	if v == "" {
		return list
	}
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// compareBranches fills Commits, Diffs, CommitCount and Compared on the
// new-MR page when ?source= (and optionally ?target=, default branch) name
// existing branches.
func (s *Server) compareBranches(r *http.Request, repo store.Repo, rc *RepoCtx, data map[string]any) {
	ctx := r.Context()
	src, tgt := r.URL.Query().Get("source"), r.URL.Query().Get("target")
	if tgt == "" {
		tgt = rc.Default
	}
	if src == "" || !gitx.SafeRevision(src) || !gitx.SafeRevision(tgt) {
		return
	}
	srcOID, ok1 := gitx.RefTarget(ctx, repo.Dir, "refs/heads/"+src)
	tgtOID, ok2 := gitx.RefTarget(ctx, repo.Dir, "refs/heads/"+tgt)
	if !ok1 || !ok2 {
		return
	}
	data["Target"] = tgt
	mb, err := gitx.MergeBase(ctx, repo.Dir, tgtOID, srcOID)
	if err != nil || mb == "" {
		return
	}
	commits, err := gitx.LogRangeLimit(ctx, repo.Dir, 100, srcOID, "^"+mb)
	if err != nil {
		return
	}
	if commits == nil {
		commits = []gitx.Commit{}
	}
	diffs, err := gitx.DiffCommits(ctx, repo.Dir, mb, srcOID)
	if err != nil {
		return
	}
	data["Commits"], data["Diffs"], data["Compared"] = commits, diffs, true
	data["CommitCount"], _ = gitx.CountCommits(ctx, repo.Dir, srcOID, "^"+mb)
}

// RangeLine is one rendered line of git range-diff output.
type RangeLine struct {
	Kind string // header-eq, header-mod, header-add, header-del, add, del, ctx
	Text string
}

var rangeHeaderRe = regexp.MustCompile(`^\s*(-|\d+):\s+(-+|[0-9a-f]+)\s+([=!<>])\s+(-|\d+):\s+(-+|[0-9a-f]+)\s?(.*)$`)

func parseRangeDiff(out string) []RangeLine {
	var lines []RangeLine
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if m := rangeHeaderRe.FindStringSubmatch(l); m != nil {
			kind := map[string]string{"=": "header-eq", "!": "header-mod", "<": "header-del", ">": "header-add"}[m[3]]
			lines = append(lines, RangeLine{Kind: kind, Text: l})
			continue
		}
		t := strings.TrimPrefix(l, "    ")
		kind := "ctx"
		switch {
		case strings.HasPrefix(t, "+"):
			kind = "add"
		case strings.HasPrefix(t, "-"):
			kind = "del"
		}
		lines = append(lines, RangeLine{Kind: kind, Text: l})
	}
	return lines
}

func (s *Server) handleMR(w http.ResponseWriter, r *http.Request, repo store.Repo, rc *RepoCtx, n int) {
	m, err := s.Store.MergeRequest(repo.ID, n)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	ctx := r.Context()
	tab := r.URL.Query().Get("tab")
	data := map[string]any{"MR": m, "Tab": tab, "LatestVersion": m.Latest().N, "InterFrom": 0, "InterTo": 0}
	latest := m.Latest()
	if m.State == "opened" {
		data["Status"] = s.Store.CheckMerge(ctx, repo, m)
	}
	participants := []string{m.Author}
	type seen struct {
		at   time.Time
		name string
	}
	var order []seen
	for _, c := range m.Comments {
		order = append(order, seen{c.At, c.Author})
	}
	for _, v := range m.Versions {
		order = append(order, seen{v.At, v.Pusher})
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].at.Before(order[j].at) })
	for _, e := range order {
		participants = appendUnique(participants, e.name)
	}
	data["Participants"] = participants
	u := currentUser(r)
	if u != nil {
		data["CanState"] = u.ID == m.AuthorID || repo.CanWrite(u.ID)
		data["CanMerge"] = repo.CanWrite(u.ID)
	}
	// base for the current view
	base := latest.Base
	if tip, ok := gitx.RefTarget(ctx, repo.Dir, "refs/heads/"+m.TargetBranch); ok && m.State == "opened" {
		if mb, err := gitx.MergeBase(ctx, repo.Dir, tip, latest.Head); err == nil {
			base = mb
		}
	}
	commits, _ := gitx.LogRange(ctx, repo.Dir, latest.Head, "^"+base)
	if base == "" {
		commits, _ = gitx.LogRange(ctx, repo.Dir, latest.Head)
	}
	if commits == nil {
		commits = []gitx.Commit{}
	}
	data["Commits"] = commits
	switch tab {
	case "commits":
	case "changes":
		data["Diffs"] = []gitx.FileDiff{}
		v := atoi(r.URL.Query().Get("version"), len(m.Versions))
		if v < 1 || v > len(m.Versions) {
			v = len(m.Versions)
		}
		ver := m.Versions[v-1]
		from := ver.Base
		if v == len(m.Versions) {
			from = base
		}
		// Interdiff: ?from=A&to=B (A<B) shows what changed between two versions.
		ia, ib := atoi(r.URL.Query().Get("from"), 0), atoi(r.URL.Query().Get("to"), 0)
		if ia >= 1 && ib <= len(m.Versions) && ia < ib {
			ver = m.Versions[ib-1]
			from = m.Versions[ia-1].Head
			v = ib
			data["InterFrom"], data["InterTo"] = ia, ib
		}
		if from != "" {
			diffs, err := gitx.DiffCommits(ctx, repo.Dir, from, ver.Head)
			if err == nil {
				data["Diffs"] = diffs
			}
		}
		data["Version"] = v
	case "versions":
		to := atoi(r.URL.Query().Get("to"), len(m.Versions))
		from := atoi(r.URL.Query().Get("from"), to-1)
		data["From"], data["To"] = from, to
		vc := map[int]int{}
		for _, v := range m.Versions {
			vb := v.Base
			if vb == "" {
				vb = base
			}
			if vb == "" {
				continue
			}
			if n, err := gitx.CountCommits(ctx, repo.Dir, v.Head, "^"+vb); err == nil {
				vc[v.N] = n
			}
		}
		data["VersionCommits"] = vc
		if len(m.Versions) >= 2 && from >= 1 && to >= 1 && from <= len(m.Versions) && to <= len(m.Versions) && from != to {
			a, b := m.Versions[from-1], m.Versions[to-1]
			baseA, baseB := a.Base, b.Base
			if baseA == "" {
				baseA = base
			}
			if baseB == "" {
				baseB = base
			}
			out, err := gitx.RangeDiff(ctx, repo.Dir, baseA, a.Head, baseB, b.Head)
			if err != nil {
				data["RangeError"] = err.Error()
			} else {
				data["Range"] = parseRangeDiff(out)
				data["RangeRaw"] = out
			}
		}
	}
	data["AGitHint"] = fmt.Sprintf("git push origin HEAD:refs/for/%s -o topic=%s", m.TargetBranch, m.Topic)
	data["FetchHint"] = fmt.Sprintf("git fetch origin refs/merge-requests/%d/head && git checkout FETCH_HEAD", n)
	s.render(w, r, "mr.html", &Page{Title: m.Title + " · !" + strconv.Itoa(n), Repo: rc, Nav: "mrs", Data: data})
}
