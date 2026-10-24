# Notes for Downstream journeys

Downstream reads this file from the target branch when it plans user journeys for a merge
request. It describes the test data the app starts with, so journeys can use real pages.

## Test data

NewGit starts with `-seed-demo` (see `downstream.yml`), which creates:

- User `demo`. There is no way to sign in as this user: sign-in needs a browser device key.
  Journeys are anonymous visitors.
- Public repository `demo/storefront`, default branch `main`, six commits, files
  `README.md`, `main.go`, `products.go`, `cart.go`, `docs/api.md`, `CHANGELOG.md`.
- Issues 1 to 6. Issue 4 is closed; issues 1, 2, 4 and 5 have comments.
- Merge requests 1 and 2, both open, with changes to `cart.go` and `products.go`.

## Pages a visitor uses (all GET, all return 200 with the data above)

| Page | Path |
|---|---|
| Home | `/` |
| Explore projects | `/explore` |
| User profile | `/demo` |
| Repository home (README) | `/demo/storefront` |
| File tree | `/demo/storefront/-/tree/main` |
| File view | `/demo/storefront/-/blob/main/cart.go` |
| Raw file | `/demo/storefront/-/raw/main/cart.go` |
| Commit history | `/demo/storefront/-/commits/main` |
| Branches | `/demo/storefront/-/branches` |
| Tags | `/demo/storefront/-/tags` |
| Push log | `/demo/storefront/-/pushes` |
| Issue list | `/demo/storefront/-/issues` |
| One issue | `/demo/storefront/-/issues/1` |
| Merge request list | `/demo/storefront/-/merge_requests` |
| One merge request | `/demo/storefront/-/merge_requests/1` |
| Health check | `/-/health` |

Repository, issue and merge request pages are served by one catch-all handler (`route` in
`internal/web/server.go`), so they all show as route `/` in Downstream's route table.
Pages that render user avatars: home, explore, profile, repository home, commit history,
issues and merge requests.
