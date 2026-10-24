package store

import (
	"time"
)

// User is an account bound to one device key.
type User struct {
	ID       string // RFC 7638 JWK thumbprint of the device public key
	Username string
	KeyX     string // base64url P-256 X coordinate
	KeyY     string // base64url P-256 Y coordinate
	Created  time.Time
	Tokens   []Token
}

// Token is a personal access token (stored hashed).
type Token struct {
	ID      string
	Name    string
	Hash    string // hex sha256 of the plaintext
	Scope   string // "read" or "write"
	Created time.Time
	Revoked bool
}

// CanWrite reports whether the token allows writes.
func (t Token) CanWrite() bool { return t.Scope == "write" }

// Repo is a hosted repository.
type Repo struct {
	ID          string
	OwnerID     string
	Owner       string // username
	Name        string
	Description string
	Created     time.Time
	Dir         string
}

// Path returns owner/name.
func (r Repo) Path() string { return r.Owner + "/" + r.Name }

// Comment is a comment on an issue or merge request.
type Comment struct {
	ID       string
	AuthorID string
	Author   string
	Body     string
	At       time.Time
	Version  int // MR version the comment was made on (0 for issues)
}

// Event is a timeline event (state change, new version, ...).
type Event struct {
	Kind    string // comment, closed, reopened, version, merged
	Actor   string
	At      time.Time
	Comment *Comment
	Version int
	Detail  string
}

// Issue is a repository issue.
type Issue struct {
	Number   int
	Title    string
	Body     string
	AuthorID string
	Author   string
	Created  time.Time
	Updated  time.Time
	State    string // opened, closed
	Comments []Comment
	Timeline []Event
}

// MRVersion is one pushed version of a merge request.
type MRVersion struct {
	N        int
	Head     string
	Base     string // merge base with target at the time of the push
	PusherID string
	Pusher   string
	At       time.Time
}

// MergeRequest is a merge request.
type MergeRequest struct {
	Number       int
	Title        string
	Description  string
	AuthorID     string
	Author       string
	SourceBranch string // empty for AGit merge requests
	TargetBranch string
	Topic        string // AGit topic
	Created      time.Time
	Updated      time.Time
	State        string // opened, closed, merged
	Versions     []MRVersion
	Comments     []Comment
	Timeline     []Event
	MergedBy     string
	MergeCommit  string
}

// Latest returns the latest version.
func (m MergeRequest) Latest() MRVersion {
	if len(m.Versions) == 0 {
		return MRVersion{}
	}
	return m.Versions[len(m.Versions)-1]
}

// RefUpdate is one ref change in a push.
type RefUpdate struct {
	Ref string `json:"ref"`
	Old string `json:"old"`
	New string `json:"new"`
}

// Push is a push-log entry: which authenticated account introduced which commits.
type Push struct {
	ByID    string      `json:"by_id"`
	By      string      `json:"by"`
	Via     string      `json:"via"` // git, web, mcp, agit
	At      time.Time   `json:"-"`
	Refs    []RefUpdate `json:"refs"`
	Commits []string    `json:"commits"`
}
