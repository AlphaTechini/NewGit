# NewGit: product and design brief

NewGit is a small GitLab-style Git host written in Go. It serves real `git clone` and `git push` over HTTP(S) to ordinary git clients, has a GitLab-like web UI, and exposes everything over MCP so an AI agent can drive it. NewGit itself has no AI features.

It is also the demo target app for Downstream, so it has to be a genuinely working product with real user journeys: sign in, create a repo, push, browse code and history, open and review merge requests, file and discuss issues.

## What makes it different

Most small forges copy GitHub's design: Git for code, an SQL database for everything else. NewGit uses a few lesser-known ideas, each created to fix a real limit of standard forges.

1. **Git is the database.** Issues, merge-request metadata, comments, the push log, users and tokens are stored inside Git as append-only operation logs: one ref per entity, one commit per operation, and the operation as JSON in the commit's tree. Precedents are Gerrit NoteDb, which has been Gerrit's only store since 3.0, and git-bug. The web UI reads from an in-memory index that is rebuilt from refs on startup, so the index can be deleted at any time without losing data. A `git clone` plus one refspec gives you the issues and review history along with the code.
2. **Push to open a merge request (AGit flow).** `git push origin HEAD:refs/for/main -o title="Fix login"` opens a merge request without a fork and without write access. It uses Git's `proc-receive` hook (Git 2.29+), which Alibaba designed and which Gitea also ships. Pushing again with `-o topic=<same>` adds a new version.
3. **Versioned merge requests with range-diff.** Every push to a merge request is kept as an immutable ref, `refs/merge-requests/N/vK`, alongside a moving `refs/merge-requests/N/head`. Reviewers compare any two versions with `git range-diff`, which shows exactly what a rebase or amend changed. GitHub and GitLab only publish a moving head ref; per-push versions exist in Gerrit's patch sets.
4. **Verified pusher.** Commit author names are self-asserted by whoever wrote the commit. NewGit records which authenticated account pushed each commit (the push log), and the commit page shows both the author and the verified pusher.
5. **Device-bound accounts, no sign-up form.** See "Accounts" below.
6. **Git speedups switched on.** After each push NewGit writes a commit-graph with changed-path Bloom filters, which Git does not write by default, and periodically repacks with reachability bitmaps. On git/git these measured roughly 5x faster file history and about 6x faster server-side clone packing.

## Accounts: one device, one account

The goal: no email, no password, no sign-up. The device you first use *is* your account. Another device, such as your phone, gets its own separate account and cannot sign in to the first one.

### Why not a hardware ID or IP address

Browsers deliberately expose no stable hardware identifier, to prevent tracking. IP addresses change. Fingerprinting is both unreliable and privacy-hostile. The correct tool is a cryptographic key that only that device holds, used to prove possession on each sign-in (proof of possession).

### What NewGit does (MVP): a non-extractable device key

1. On first visit, the browser generates an ECDSA P-256 key pair with WebCrypto `generateKey(..., extractable: false)`. The private key cannot be read or exported by any script, including NewGit's own. The browser keeps it in IndexedDB on that device only. WebCrypto requires a secure context, so use HTTPS or localhost.
2. The browser sends the public key (as a JWK) and a chosen username. The server signs it up after checking a signature over a one-time server challenge. The account ID is the key's RFC 7638 JWK thumbprint.
3. Every later sign-in is automatic and silent. The page asks for a challenge, the device signs it, and the server verifies it against the stored public key and sets a session cookie. There is nothing to type.
4. A different device, or a different browser profile, has a different key and therefore a different account.

This is the same proof-of-possession pattern as Google's Device Bound Session Credentials (DBSC). It works in every modern browser today.

### Hardware-backed upgrade (later)

- **WebAuthn platform credentials with the BE flag enforced.** WebAuthn authenticator data carries a Backup Eligible (BE) bit. BE=0 means a single-device credential, and a server can reject BE=1 (synced) credentials. The catch is that Apple's passkeys are always synced, so iPhone and Mac users could not register under a strict BE=0 rule. NewGit should therefore offer this as an optional upgrade rather than the only way in. The flags are also self-asserted by the authenticator unless attestation is checked.
- **DBSC.** Shipped in Chrome 146 on Windows, backed by the TPM, with macOS (Secure Enclave) announced. It binds session refresh to a key that never leaves the hardware. It is not cross-browser yet.

### Honest limits

- The MVP device key is held by the browser in software, not in a TPM or Secure Enclave.
- Clearing the site's data deletes the key, and there is deliberately no recovery path, because a recovery path would be a second way in.
- Git over HTTPS and MCP cannot use a browser key. For those, the signed-in device issues personal access tokens: random, shown once, stored only as a SHA-256 hash, revocable, and labelled with the device that issued them. A token is a bearer secret, so it can be copied to another machine. That is inherent to HTTPS Git auth. FIDO-backed SSH keys (`ed25519-sk`) are the hardware-bound option for a later version.

## Architecture

- **One Go binary** (`newgit`) with subcommands `serve` and `hook`. It requires `git` 2.38+ on PATH (for `merge-tree --write-tree`); 2.45+ is recommended.
- **Transport:** NewGit runs `git upload-pack` and `git receive-pack --stateless-rpc` directly through `os/exec`, the way Gitea does. It does not use Go's `net/http/cgi`, which rejects chunked request bodies and so fails pushes over 1 MiB. Clone and fetch are anonymous. Push sends a `401` with `WWW-Authenticate: Basic` and accepts a personal access token as the password. Protocol v2 is passed through via the `Git-Protocol` header, and partial clone is enabled.
- **Git reads:** `git cat-file`, `ls-tree`, `log`, `diff` and `range-diff`, all run from the git binary so commit-graph and bitmaps apply. go-git is not used: it cannot open reftable repos and ignores Bloom filters and bitmaps.
- **Merges:** `git merge-tree --write-tree`, then `commit-tree`, then `update-ref <ref> <new> <old>`. No worktree is involved, so untrusted content is never checked out.
- **Hooks:** an app-owned global git config sets `core.hooksPath` to NewGit's hook directory. Each hook script runs `newgit hook <name>`.
  - `pre-receive` enforces ref policy: clients can never write `refs/newgit/*` or `refs/merge-requests/*`, and non-owners may only push to `refs/for/*`.
  - `proc-receive` implements AGit by calling back into the server over a local Unix socket.
- **Storage on disk:**
  - `data/repos/<id>.git` holds bare repositories, named by ID so renames and path traversal don't matter.
  - `data/system.git` holds users, tokens and the repo registry, also as op-log refs. It is never served.
  - `data/gitconfig` is the app-owned git config. Every git child runs with `GIT_CONFIG_NOSYSTEM=1` and `GIT_DIR` set.
- **Refs layout inside a repo:**
  - `refs/heads/*`, `refs/tags/*`: normal.
  - `refs/merge-requests/N/head`, `refs/merge-requests/N/vK`: MR code versions.
  - `refs/newgit/issues/N`, `refs/newgit/mr/N`, `refs/newgit/pushlog`: op-logs.
- **Web UI:** server-rendered `html/template` with a GitLab-style layout (top bar plus a project sidebar), chroma for syntax highlighting, and goldmark for Markdown (raw HTML disabled). The only JavaScript is device sign-in.
- **MCP:** the official Go SDK (`github.com/modelcontextprotocol/go-sdk`) serves streamable HTTP at `/mcp`, authenticated with a personal access token as a Bearer token.

## Sources

- Gerrit NoteDb: https://gerrit-review.googlesource.com/Documentation/note-db.html
- git-bug data model: https://github.com/git-bug/git-bug/blob/trunk/doc/design/data-model.md
- AGit flow: https://git-repo.info/en/2020/03/agit-flow-and-git-repo/ and Gitea: https://docs.gitea.com/usage/agit
- githooks `proc-receive`: https://git-scm.com/docs/githooks
- git range-diff: https://git-scm.com/docs/git-range-diff
- Commit-graph config: https://github.com/git/git/blob/master/Documentation/config/commitgraph.adoc
- WebAuthn BE/BS flags: https://www.w3.org/TR/webauthn-3/ and https://mojoauth.com/blog/webauthn-be-bs-flags-synced-vs-device-bound-passkey
- Apple and BE=0: https://developer.apple.com/forums/thread/813844
- DBSC in Chrome 146: https://helpnetsecurity.com/2026/04/10/google-chrome-device-bound-session-credentials
- WebCrypto generateKey: https://developer.mozilla.org/en-US/docs/Web/API/SubtleCrypto/generateKey
- Go net/http/cgi chunked bodies: https://github.com/golang/go/blob/master/src/net/http/cgi/host.go
- MCP Go SDK: https://github.com/modelcontextprotocol/go-sdk
