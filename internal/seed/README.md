# internal/seed

Demo data for `newgit serve -seed-demo`. A fresh data directory has no users or
repositories, so a demo or a load test would only see empty pages. With the flag, the
server creates one user (`demo`), a repository (`demo/storefront`) with six commits, six
issues with comments, and two open merge requests, then starts as usual.

- The seed runs only when the demo user does not exist yet, so restarting on the same data
  directory changes nothing.
- The seed account has no device key, so nobody can sign in as it. Everything it creates is
  public and readable without signing in.
- It writes through the normal store functions (`CreateUser`, `CreateRepo`, `CommitFiles`,
  `CreateIssue`, `ProposeChange`), so the data is the same as data made through the UI.

To find the seeding steps visit [seed.go](seed.go).
The demo repository's files, issues and merge requests can be found in [data.go](data.go).
