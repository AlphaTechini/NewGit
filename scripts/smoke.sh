#!/usr/bin/env bash
# End-to-end smoke test: builds NewGit, starts it on a temp data dir, and
# drives it with a stock git client and the headless device client.
# Needs: go, git >= 2.38, node >= 18, curl.
set -eu  # no pipefail: checks pipe failing git commands into grep
cd "$(dirname "$0")/.."
ROOT=$(pwd)
PORT=${PORT:-18080}
B="http://localhost:$PORT"
TMP=$(mktemp -d)
trap 'kill $SERVER 2>/dev/null || true; rm -rf "$TMP"' EXIT
export NO_PROXY=localhost,127.0.0.1 no_proxy=localhost,127.0.0.1 GIT_TERMINAL_PROMPT=0
DC="node $ROOT/scripts/device-client.mjs"
pass() { printf '  \033[32mok\033[0m  %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; exit 1; }

echo "building..."
go build -o "$TMP/newgit" ./cmd/newgit
"$TMP/newgit" -addr "127.0.0.1:$PORT" -data "$TMP/data" -base-url "$B" >"$TMP/server.log" 2>&1 &
SERVER=$!
for _ in $(seq 50); do curl -sf "$B/-/health" >/dev/null && break; sleep 0.1; done

echo "accounts (device keys)"
if $DC register "$B" alice "$TMP/alice.json" >/dev/null; then pass "alice registered with a device key"; else fail "alice registered with a device key"; fi
if $DC register "$B" bob "$TMP/bob.json" >/dev/null; then pass "bob registered with a different device key"; else fail "bob registered with a different device key"; fi
$DC register "$B" alice "$TMP/x.json" >/dev/null 2>&1 && fail "duplicate username accepted" || pass "duplicate username rejected"
C=$($DC login "$B" "$TMP/alice.json")
if curl -s -H "Cookie: $C" "$B/" | grep -q 'data-signed-in="1"'; then pass "alice signs in by signing a challenge"; else fail "alice signs in by signing a challenge"; fi
AT=$($DC token "$B" "$TMP/alice.json" laptop write)
BT=$($DC token "$B" "$TMP/bob.json" laptop write)
if [[ $AT == ngp_* && $BT == ngp_* ]]; then pass "personal access tokens issued"; else fail "personal access tokens issued"; fi

echo "repositories and git transport"
CSRF=$(curl -s -H "Cookie: $C" "$B/-/new" | grep -o 'name="csrf" value="[0-9a-f]*"' | head -1 | cut -d'"' -f4)
curl -s -o /dev/null -H "Cookie: $C" -d "csrf=$CSRF&name=demo&description=Demo&readme=on" "$B/-/new"
if git clone -q "$B/alice/demo.git" "$TMP/w"; then pass "anonymous clone"; else fail "anonymous clone"; fi
cd "$TMP/w" && git config user.name "Alice" && git config user.email a@example.com
git push origin main >/dev/null 2>&1 && fail "unauthenticated push accepted" || pass "unauthenticated push asks for credentials (401)"
head -c 5000000 /dev/urandom > big.bin && git add big.bin && git commit -qm "5MB file"
if git push -q "http://x:$AT@localhost:$PORT/alice/demo.git" main; then pass "5 MB push (chunked request body)"; else fail "5 MB push (chunked request body)"; fi
git checkout -qb feature && echo "// feature" > f.go && git add f.go && git commit -qm "Feature" && git push -q "http://x:$AT@localhost:$PORT/alice/demo.git" feature
git checkout -q main
if git commit -q --allow-empty -m "bob direct" && git push "http://x:$BT@localhost:$PORT/alice/demo.git" HEAD:refs/heads/main 2>&1 | grep -q "pre-receive hook declined"; then pass "non-owner cannot push to branches"; else fail "non-owner cannot push to branches"; fi
if git push "http://x:$AT@localhost:$PORT/alice/demo.git" main:refs/newgit/issues/9 2>&1 | grep -q "declined"; then pass "server-managed refs cannot be pushed"; else fail "server-managed refs cannot be pushed"; fi
if git clone -q --filter=blob:none "$B/alice/demo.git" "$TMP/partial"; then pass "partial clone (blob:none)"; else fail "partial clone (blob:none)"; fi
if git clone -q --depth 1 "$B/alice/demo.git" "$TMP/shallow"; then pass "shallow clone"; else fail "shallow clone"; fi

echo "merge requests"
git checkout -q -b bob-change origin/main 2>/dev/null || git checkout -q -b bob-change main
echo "# Contributing" > CONTRIBUTING.md && git add . && git commit -qm "Add contributing"
OUT=$(git push "http://x:$BT@localhost:$PORT/alice/demo.git" HEAD:refs/for/main -o title="Add contributing guide" 2>&1)
if echo "$OUT" | grep -q "Created merge request !1"; then pass "AGit: push to refs/for/main opened !1"; else fail "AGit: push to refs/for/main opened !1"; fi
echo "Be kind." >> CONTRIBUTING.md && git commit -q --amend -am "Add contributing (v2)"
if git push "http://x:$BT@localhost:$PORT/alice/demo.git" HEAD:refs/for/main -o topic=mr-1 2>&1 | grep -q "version 2"; then pass "AGit: second push recorded v2"; else fail "AGit: second push recorded v2"; fi
if git ls-remote "$B/alice/demo.git" | grep -q "refs/merge-requests/1/v2"; then pass "immutable version refs exist"; else fail "immutable version refs exist"; fi
if curl -s "$B/alice/demo/-/merge_requests/1?tab=versions" | grep -q 'class="header-'; then pass "range-diff between versions renders"; else fail "range-diff between versions renders"; fi
CSRF=$(curl -s -H "Cookie: $C" "$B/alice/demo/-/merge_requests/1" | grep -o 'name="csrf" value="[0-9a-f]*"' | head -1 | cut -d'"' -f4)
curl -s -o /dev/null -H "Cookie: $C" -d "csrf=$CSRF" "$B/alice/demo/-/merge_requests/1/merge"
if curl -s "$B/alice/demo/-/merge_requests/1" | grep -q 'pill merged'; then pass "owner merged !1 (merge-tree, no worktree)"; else fail "owner merged !1 (merge-tree, no worktree)"; fi
CSRF=$(curl -s -H "Cookie: $C" "$B/alice/demo/-/merge_requests/new" | grep -o 'name="csrf" value="[0-9a-f]*"' | head -1 | cut -d'"' -f4)
curl -s -o /dev/null -H "Cookie: $C" -d "csrf=$CSRF&source=feature&target=main&title=Feature" "$B/alice/demo/-/merge_requests/new"
if curl -s "$B/alice/demo/-/merge_requests/2" | grep -q "Ready to merge"; then pass "branch MR !2 opened and mergeable"; else fail "branch MR !2 opened and mergeable"; fi

echo "issues and traceability"
CB=$($DC login "$B" "$TMP/bob.json")
CSRF=$(curl -s -H "Cookie: $CB" "$B/alice/demo/-/issues/new" | grep -o 'name="csrf" value="[0-9a-f]*"' | head -1 | cut -d'"' -f4)
curl -s -o /dev/null -H "Cookie: $CB" --data-urlencode "csrf=$CSRF" --data-urlencode "title=Login breaks" -d "body=steps" "$B/alice/demo/-/issues/new"
if curl -s "$B/alice/demo/-/issues/1" | grep -q "Login breaks"; then pass "issue opened"; else fail "issue opened"; fi
if git fetch -q origin '+refs/newgit/*:refs/newgit/*' && git rev-parse -q --verify refs/newgit/issues/1 >/dev/null; then pass "issue history fetched with plain git"; else fail "issue history fetched with plain git"; fi
SHA=$(git rev-parse origin/main)
if curl -s "$B/alice/demo/-/commit/$SHA" | grep -q "✓ @alice"; then pass "commit page shows verified pusher"; else fail "commit page shows verified pusher"; fi

echo "MCP"
MCP() { curl -s -H "Authorization: Bearer $AT" -H "Content-Type: application/json" -H "Accept: application/json, text/event-stream" -d "$1" "$B/mcp"; }
if MCP '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}' | grep -q '"protocolVersion":"2025-06-18"'; then pass "initialize"; else fail "initialize"; fi
if MCP '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' | grep -q '"propose_change"'; then pass "tools/list"; else fail "tools/list"; fi
if MCP '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"propose_change","arguments":{"repo":"alice/demo","message":"Agent fix","files":[{"path":"fix.txt","content":"fixed\n"}]}}}' | grep -q "Opened merge request"; then pass "propose_change opened an MR"; else fail "propose_change opened an MR"; fi

echo "restart rebuilds the index from git refs"
kill $SERVER; wait $SERVER 2>/dev/null || true
"$TMP/newgit" -addr "127.0.0.1:$PORT" -data "$TMP/data" -base-url "$B" >>"$TMP/server.log" 2>&1 &
SERVER=$!
for _ in $(seq 50); do curl -sf "$B/-/health" >/dev/null && break; sleep 0.1; done
if [[ $(curl -s "$B/alice/demo/-/merge_requests?state=all" | grep -o 'merge_requests/[0-9]*"' | sort -u | wc -l) -ge 3 ]]; then pass "merge requests survive restart"; else fail "merge requests survive restart"; fi

echo "all checks passed"
