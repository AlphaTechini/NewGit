// Package seed fills an empty data directory with a small demo project, so a
// fresh server has real pages to browse. It is used by `newgit serve
// -seed-demo` for demos and for load tests that start from an empty data dir.
package seed

import (
	"context"
	"fmt"

	"github.com/AlphaTechini/newgit/internal/store"
)

const (
	userID   = "seed-demo"
	userName = "demo"
	repoName = "storefront"
	via      = "seed"
)

// Demo creates the demo user, a repository with history, issues and merge
// requests. It does nothing if the demo user already exists, so restarting on
// the same data dir is safe.
func Demo(ctx context.Context, st *store.Store, baseURL string) error {
	if _, ok := st.UserByName(userName); ok {
		return nil
	}
	// The seed account has no device key, so nobody can sign in as it.
	u, err := st.CreateUser(ctx, userID, userName, "", "")
	if err != nil {
		return fmt.Errorf("seed user: %w", err)
	}
	repo, err := st.CreateRepo(ctx, *u, repoName, "A small shop API used to demo NewGit.")
	if err != nil {
		return fmt.Errorf("seed repo: %w", err)
	}
	for i, c := range history {
		if _, err := st.CommitFiles(ctx, repo.ID, *u, via, "main", "", c.message, c.files); err != nil {
			return fmt.Errorf("seed commit %d: %w", i+1, err)
		}
	}
	for _, is := range issues {
		created, err := st.CreateIssue(ctx, repo.ID, *u, is.title, is.body)
		if err != nil {
			return fmt.Errorf("seed issue %q: %w", is.title, err)
		}
		for _, c := range is.comments {
			if _, err := st.CommentIssue(ctx, repo.ID, created.Number, *u, c); err != nil {
				return fmt.Errorf("seed comment on issue %d: %w", created.Number, err)
			}
		}
		if is.closed {
			if err := st.SetIssueState(ctx, repo.ID, created.Number, *u, "closed"); err != nil {
				return fmt.Errorf("seed close issue %d: %w", created.Number, err)
			}
		}
	}
	for _, m := range mergeRequests {
		mr, err := st.ProposeChange(ctx, repo.ID, *u, via, "main", m.title, m.description, m.title, m.files, baseURL)
		if err != nil {
			return fmt.Errorf("seed merge request %q: %w", m.title, err)
		}
		for _, c := range m.comments {
			if _, err := st.CommentMergeRequest(ctx, repo.ID, mr.Number, *u, c); err != nil {
				return fmt.Errorf("seed comment on merge request %d: %w", mr.Number, err)
			}
		}
	}
	return nil
}
