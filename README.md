# NewGit

A small, working, GitLab-style Git host written in Go. Real `git clone` and `git push` over HTTP(S) with any stock git client, a GitLab-like web UI, merge requests, issues, and an MCP endpoint so AI agents can use it too. NewGit itself has no AI features.

It is built on a few lesser-known ideas, each of which was created to fix a real limit of standard forges:

| Idea | What it fixes | Where |
|---|---|---|
| **Git is the database.** Issues, merge-request metadata, comments, the push log, users and tokens are append-only operation logs stored in git refs (the Gerrit NoteDb / git-bug family). The UI reads an in-memory index rebuilt from refs at startup. | Collaboration data locked in a forge's SQL database. Here `git fetch` brings it along, and backups are just git. | `internal/oplog`, `internal/store` |
| **Push to open a merge request (AGit flow).** `git push origin HEAD:refs/for/main -o title="..."` opens an MR with no fork and no write access, via git's `proc-receive` hook. | Fork-per-contributor workflows. | `internal/hooks`, `internal/store/agit.go` |
| **Versioned merge requests with range-diff.** Every push to an MR is kept as an immutable ref `refs/merge-requests/N/vK`. Reviewers compare any two versions with `git range-diff`. | GitHub and GitLab expose only a moving head, so a force-push hides what changed. | `internal/web/handlers_collab.go` |
| **Verified pusher.** The server records which authenticated account pushed each commit. Commit pages show it next to the self-asserted author. | Anyone can write any name in `git commit --author`. | `refs/newgit/pushlog` |
| **Device-bound accounts, no passwords.** The browser makes a non-extractable WebCrypto key, and the device *is* the account (proof of possession on each sign-in). | Passwords, emails and sign-up forms. | `internal/auth`, `internal/web/static/device.js` |
| **Git's speedups switched on.** After each push the server writes a commit-graph with changed-path Bloom filters (not written by default), and periodically repacks with reachability bitmaps. | Slow `log -- path` and clone counting on hosts that leave these off. | `internal/store/store.go` |

Server-side merges use `git merge-tree --write-tree`, so nothing is ever checked out.

See [details.md](details.md) for the design and sources, and [requirements.md](requirements.md) for the scope.

## Quick start

Requirements: Go 1.24+ and **git 2.38+** on the server's PATH. For the smoke test you also need Node 18+.

```sh
make run            # or: go run ./cmd/newgit -addr :8080 -data ./data
open http://localhost:8080
```

Flags (or env vars): `-addr` (`NEWGIT_ADDR`), `-data` (`NEWGIT_DATA`), `-base-url` (`NEWGIT_BASE_URL`, the public URL), `-max-push-mb`.

Sign-in uses WebCrypto, which browsers only allow in a secure context: `http://localhost` or HTTPS. To demo on another machine, put it behind HTTPS (for example a Cloudflare Tunnel) and set `-base-url https://your.host`.

## Using it

1. **Sign in with this device.** Click *Sign in* and pick a username. Your browser makes a key that cannot be copied out of it, and every later visit signs in silently. A phone or another browser gets its own separate account.
2. **Create a project** with *New project*. Anyone can browse and clone it.
3. **Get a token** for git and agents on *This device*. Use it as the password when git asks. The username can be anything.
4. **Push**
   ```sh
   git remote add origin http://localhost:8080/<you>/<project>.git
   git push -u origin main
   ```
5. **Open a merge request.** Do it from a branch in the UI (owner), or from anyone's clone by pushing:
   ```sh
   git push origin HEAD:refs/for/main -o title="Fix login" -o description="Why"
   # amend or rebase, then push a new version of the same MR:
   git push origin HEAD:refs/for/main -o topic=mr-1
   ```
   The MR page has Overview, Commits, Changes (any version) and Versions (range-diff between any two versions). The owner merges once `merge-tree` reports no conflicts.
6. **Issues** are numbered per project and support Markdown, comments, close and reopen.
7. **Take the collaboration data with you:**
   ```sh
   git fetch origin '+refs/newgit/*:refs/newgit/*'
   git log --format='%s by %an' refs/newgit/issues/1
   ```

## MCP (AI agents)

Endpoint: `POST /mcp` (streamable HTTP, JSON responses). Authenticate with a token as `Authorization: Bearer <token>`.

```json
{ "mcpServers": { "newgit": { "type": "http", "url": "http://localhost:8080/mcp",
  "headers": { "Authorization": "Bearer ngp_..." } } } }
```

Tools: `whoami`, `list_repos`, `create_repo`, `list_branches`, `list_tree`, `read_file`, `list_commits`, `get_commit`, `list_issues`, `get_issue`, `create_issue`, `comment_issue`, `set_issue_state`, `list_merge_requests`, `get_merge_request`, `get_merge_request_diff`, `get_merge_request_range_diff`, `create_merge_request`, `comment_merge_request`, `set_merge_request_state`, `merge_merge_request`, `commit_files` (owner), `propose_change` (anyone: commits on top of a branch and opens an MR).

Read-only tokens can call only the read tools. Permissions match the web UI.

## Verify it

```sh
make smoke
```

`scripts/smoke.sh` builds the binary, starts it on a temporary data directory, and runs 26 checks with a stock git client:

- device registration and sign-in
- tokens
- anonymous clone, partial clone and shallow clone
- the 401 challenge
- a 5 MB push
- ref policy
- AGit v1 and v2
- range-diff
- merges
- issues
- fetching issue history with git
- the verified pusher
- MCP
- the index rebuilt after a restart

`scripts/device-client.mjs` is a headless device client (same protocol as the browser), useful for scripted user journeys.

### UI screenshots

The JavaScript tooling uses pnpm. Playwright drives Chromium against a server seeded with demo data:

```sh
pnpm install
pnpm screenshots                 # every page in light, dark and 390px mobile, written to screenshots/
pnpm screenshots --keep          # leave the seeded server running to click around
```

The seed has two accounts, a project with branches, tags, a versioned AGit merge request, a branch merge request and issues. Set `NEWGIT_BIN` to build the binary somewhere other than `bin/newgit`. The run fails on any 5xx or template error.

## Design

The UI is server-rendered with no framework.

- **Theme:** design tokens with light and dark themes. Follows the system setting, or set it from the theme menu (a cookie, so no flash on load).
- **Font:** Inter Variable, self-hosted under `internal/web/static/fonts/` (SIL OFL 1.1).
- **Icons:** an SVG sprite at `internal/web/static/icons.svg`.
- **Style guide:** `/-/static/styleguide.html` shows every component in both themes.
- **JavaScript:** the only script is `device.js`. It handles device sign-in, copy buttons and small conveniences such as Escape closing menus and Ctrl+Enter to comment.

## Layout

```
cmd/newgit/            main: flags, app-owned git config, hook scripts, hook socket, maintenance
internal/gitx/         every git invocation (os/exec), log/tree/diff/merge-tree/range-diff parsing
internal/oplog/        entity = ref, op = commit with op.json, compare-and-swap appends
internal/store/        users, tokens, repos, issues, MRs, versions, push log, AGit, merges
internal/auth/         device-key proofs (ECDSA P-256), sessions, CSRF
internal/githttp/      smart HTTP: upload-pack / receive-pack --stateless-rpc, 401 challenge
internal/hooks/        pre-receive policy, proc-receive (AGit) over pkt-line
internal/web/          GitLab-style UI: templates, CSS, device.js
internal/mcpserver/    MCP JSON-RPC handler and tools
scripts/               smoke test, headless device client
```

On disk, everything lives under `data/`:

- `repos/<id>.git`: bare repos, named by ID.
- `system.git`: users, tokens and the repo registry as op-log refs; never served.
- `gitconfig`: the app-owned config every git child uses, with `GIT_CONFIG_NOSYSTEM=1`.
- `hooks/`
- `session.key`

## Security notes

- **Inputs:**
  - Repo names are validated.
  - Repos are stored by ID.
  - User-supplied revisions go after `--end-of-options`.
  - Every git child runs with `GIT_DIR`, no system config, and an app-owned global config.
- **Pushes:**
  - `receive.fsckObjects` and `receive.maxInputSize` are on.
  - Clients can never write `refs/newgit/*` or `refs/merge-requests/*`.
  - Non-owners can only push to `refs/for/*`.
- **Raw content:** Raw files are served as `text/plain` or as attachments with a sandbox CSP. Markdown never renders raw HTML.
- **Web pages:** Pages send a strict CSP (no inline scripts), and forms are CSRF-protected.
- **Tokens:** Tokens are stored as SHA-256 hashes and shown once.
- **Device keys:**
  - A device key lives in the browser's IndexedDB in software, not in a TPM. Clearing site data loses the account, and by design there is no recovery path.
  - A hardware-backed upgrade (WebAuthn credentials with the BE flag checked, or DBSC) is described in [details.md](details.md).

## Not in this version

SSH transport, forks, private repos and collaborators, webhooks and CI, LFS, code search, inline line comments, approvals and merge queues. See [requirements.md](requirements.md).
