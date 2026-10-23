# NewGit: requirements

Scope key: **MVP** ships in the first version. **Later** is designed for but not built yet.

## 1. Accounts and auth

| ID | Requirement | Scope |
|---|---|---|
| A1 | First visit generates a non-extractable ECDSA P-256 device key in the browser (WebCrypto, IndexedDB). The only input is a username. | MVP |
| A2 | Registration and sign-in verify a signature over a one-time, short-lived server challenge. The account ID is the JWK thumbprint of the device public key. | MVP |
| A3 | Sign-in is automatic on every visit from the same device. A different device or browser profile creates a separate account. | MVP |
| A4 | Sessions are HMAC-signed, HttpOnly, SameSite=Lax cookies. Every form POST carries a CSRF token. | MVP |
| A5 | The signed-in device creates personal access tokens (name, scope: read or write). Tokens are shown once, stored as a SHA-256 hash, revocable, and labelled with the issuing device. | MVP |
| A6 | Git push and MCP authenticate with a token. Git uses HTTP Basic with the token as the password; MCP uses a Bearer token. | MVP |
| A7 | Optional WebAuthn upgrade that rejects synced credentials (BE=1). Optional DBSC session binding on Chrome. | Later |
| A8 | FIDO-backed SSH keys (`ed25519-sk`) for git over SSH. | Later |

## 2. Repositories and git transport

| ID | Requirement | Scope |
|---|---|---|
| R1 | A signed-in user creates a repository (name, description). The name matches `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`. It is stored as a bare repo by ID. | MVP |
| R2 | `git clone`, `fetch` and `pull` work anonymously over smart HTTP with a stock git client. Protocol v2, shallow clone and partial clone (`--filter=blob:none`) all work. | MVP |
| R3 | `git push` by the owner updates branches and tags. An unauthenticated push gets `401` + `WWW-Authenticate: Basic`. Pushes over 1 MiB work. | MVP |
| R4 | Clients can never write `refs/newgit/*` or `refs/merge-requests/*` (enforced by pre-receive). | MVP |
| R5 | `receive.maxInputSize` and `receive.fsckObjects` are on. Every git child runs with `GIT_DIR`, `GIT_CONFIG_NOSYSTEM=1` and the app-owned config. User-supplied refs are passed after `--end-of-options`. | MVP |
| R6 | After each push, write a commit-graph with changed-path Bloom filters. Periodically run maintenance and repack with bitmaps. | MVP |
| R7 | Collaborators, groups, private repos, forks. | Later |

## 3. Browsing (GitLab-style UI)

| ID | Requirement | Scope |
|---|---|---|
| B1 | GitLab-like layout: top bar, project sidebar (Overview, Files, Commits, Branches, Issues, Merge requests), and an HTTPS clone URL box. | MVP |
| B2 | Project overview shows the file tree of the default branch and the rendered README. | MVP |
| B3 | Tree view and file view at any branch, tag or commit. File view is syntax-highlighted; Markdown files are rendered; binary files get a notice. | MVP |
| B4 | Commit list per branch, paginated, with author, date and short SHA. | MVP |
| B5 | Commit page shows message, author, committer, parents, **verified pusher** (from the push log), and a per-file unified diff. | MVP |
| B6 | Branch list with last commit and ahead/behind counts against the default branch. | MVP |
| B7 | Explore page listing all repositories, plus a user page listing that user's repositories. | MVP |
| B8 | Blame, code search, tags page, compare page. | Later |

## 4. Issues

| ID | Requirement | Scope |
|---|---|---|
| I1 | Signed-in users open issues (title, Markdown description). Issues are numbered per repo. | MVP |
| I2 | Comment on issues. Close and reopen (by author or repo owner). | MVP |
| I3 | Issues are stored as op-log entities in `refs/newgit/issues/N`. They are fetchable with `git fetch origin '+refs/newgit/*:refs/newgit/*'`. | MVP |
| I4 | Labels, assignees, milestones, search. | Later |

## 5. Merge requests

| ID | Requirement | Scope |
|---|---|---|
| M1 | The repo owner opens an MR from a source branch to a target branch in the UI. | MVP |
| M2 | Anyone with a token opens an MR by pushing to `refs/for/<target>` with `-o title=... -o description=...` (AGit, via `proc-receive`). The client sees the MR number and URL. | MVP |
| M3 | Each push to an MR creates an immutable version ref `refs/merge-requests/N/vK` and moves `refs/merge-requests/N/head`. Branch-based MRs get a new version when their source branch is pushed. AGit MRs get one when the same `-o topic=` is pushed again. | MVP |
| M4 | MR page tabs: Overview (description, comments, merge status), Commits, Changes (diff against the merge base), Versions (range-diff between any two versions). | MVP |
| M5 | Comments on the MR are tagged with the version they were made on and shown as "from vK" on later versions. | MVP |
| M6 | The owner merges with a merge commit created through `merge-tree --write-tree` + `commit-tree` + compare-and-swap `update-ref`. Conflicts are detected and shown, and the merge is blocked. Close and reopen are supported. | MVP |
| M7 | MR metadata lives in `refs/newgit/mr/N` (op-log). | MVP |
| M8 | Inline line comments ported across versions; approvals; merge queue; squash and rebase options. | Later |

## 6. MCP server

| ID | Requirement | Scope |
|---|---|---|
| P1 | Streamable HTTP at `/mcp`, Bearer token auth. | MVP |
| P2 | Tools: `whoami`, `list_repos`, `create_repo`, `list_branches`, `list_tree`, `read_file`, `list_commits`, `get_commit`, `list_issues`, `get_issue`, `create_issue`, `comment_issue`, `set_issue_state`, `list_merge_requests`, `get_merge_request`, `create_merge_request`, `get_merge_request_diff`, `get_merge_request_range_diff`, `comment_merge_request`, `merge_merge_request`, `set_merge_request_state`, `commit_files` (owner: commit straight to a branch), `propose_change` (anyone: commit on top of a target and open an MR, same path as AGit). | MVP |
| P3 | Permissions are the same as the web UI's: read is open to all, writes need the right role. | MVP |

## 7. Storage and operations

| ID | Requirement | Scope |
|---|---|---|
| S1 | No external database. All state lives in git repos under `data/`, plus a random session secret file. | MVP |
| S2 | An in-memory index is rebuilt from refs on startup. Deleting nothing but the process loses no data. | MVP |
| S3 | Op-log writes use compare-and-swap `update-ref` and per-repo locks. | MVP |
| S4 | Single binary. Configured by flags `-addr`, `-data` and `-base-url`. | MVP |
| S5 | Webhooks, CI, LFS, reftable by default, object-storage backends. | Later |

## Acceptance checks (MVP)

1. Open the site in two different browsers. Each gets its own account with no password entered, and each stays signed in after a reload.
2. Create a repo, then `git clone`, commit, and `git push` with a token. A 5 MB file push succeeds.
3. Browse files, history and a commit. The commit page shows "pushed by @user".
4. Push a branch and open an MR in the UI. Push again: v2 appears and Versions shows the range-diff. Merge it.
5. From a second account: `git push origin HEAD:refs/for/main -o title=Test` prints the new MR, and `main` is untouched.
6. Open an issue, comment, close it. `git fetch origin '+refs/newgit/*:refs/newgit/*'` brings the issue history down.
7. Connect an MCP client with a token, list repos, read a file, open an issue, and propose a change that becomes an MR.
