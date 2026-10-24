package web

import (
	"context"
	"net/http"
	"sync"

	"github.com/AlphaTechini/newgit/internal/gitx"
)

const refsKey ctxKey = 2

// refsMemo holds one ref snapshot for the lifetime of a request, so the repo
// header, ref/path resolution and the page itself share a single git call.
type refsMemo struct {
	once sync.Once
	refs gitx.Refs
}

// withRefsMemo enables the snapshot for read-only requests. Requests that
// change refs (merges, branch deletes) always read fresh.
func withRefsMemo(r *http.Request) *http.Request {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), refsKey, &refsMemo{}))
}

// repoRefs returns the request's ref snapshot. A failed read gives an empty
// snapshot, which pages already treat as an empty repository.
func repoRefs(r *http.Request, gitDir string) gitx.Refs {
	m, ok := r.Context().Value(refsKey).(*refsMemo)
	if !ok {
		refs, _ := gitx.ReadRefs(r.Context(), gitDir)
		return refs
	}
	m.once.Do(func() { m.refs, _ = gitx.ReadRefs(r.Context(), gitDir) })
	return m.refs
}

// commitCounts caches "how many commits reach this tip". The answer for a
// commit id never changes, so entries never go stale; the map is just reset
// when it grows too large.
type commitCounts struct {
	mu sync.Mutex
	m  map[string]int
}

const maxCommitCounts = 4096

func (c *commitCounts) get(ctx context.Context, gitDir, oid string) int {
	c.mu.Lock()
	n, ok := c.m[oid]
	c.mu.Unlock()
	if ok {
		return n
	}
	n, err := gitx.CountCommits(ctx, gitDir, oid)
	if err != nil {
		return 0
	}
	c.mu.Lock()
	if c.m == nil || len(c.m) >= maxCommitCounts {
		c.m = map[string]int{}
	}
	c.m[oid] = n
	c.mu.Unlock()
	return n
}
