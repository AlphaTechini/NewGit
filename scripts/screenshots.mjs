#!/usr/bin/env node
// Builds NewGit, starts it on a temporary data directory, seeds a demo
// project through the same flows as scripts/smoke.sh (device-client.mjs,
// stock git over HTTP, AGit pushes, the web forms) and captures full-page
// Playwright screenshots of the main pages into screenshots/.
//
//   pnpm screenshots                  write screenshots/*.png
//   pnpm screenshots --out DIR        write somewhere else
//   pnpm screenshots --keep           leave the server running, print its URL
//
// Exits non-zero when a page returns 5xx, shows a template error, or does not
// return the status it is expected to (200, or 404 for the error page).
import { execFileSync, spawn, spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, openSync, rmSync, statSync, writeFileSync } from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { chromium } from "playwright";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
// NEWGIT_BIN lets parallel runs build to separate paths.
const BIN = process.env.NEWGIT_BIN ? path.resolve(process.env.NEWGIT_BIN) : path.join(ROOT, "bin", "newgit");
const DEVICE_CLIENT = path.join(ROOT, "scripts", "device-client.mjs");
const argv = process.argv.slice(2);
const KEEP = argv.includes("--keep");
const outFlag = argv.indexOf("--out");
if (outFlag !== -1 && !argv[outFlag + 1]) {
  console.error("--out needs a directory");
  process.exit(2);
}
const OUT_DIR = path.resolve(outFlag !== -1 ? argv[outFlag + 1] : path.join(ROOT, "screenshots"));

// Keep local requests off any configured proxy.
process.env.NO_PROXY = "localhost,127.0.0.1";
process.env.no_proxy = process.env.NO_PROXY;
process.env.GIT_TERMINAL_PROMPT = "0";

const USERS = {
  alice: { name: "Alice Chen", email: "alice@example.com" },
  bob: { name: "Bob Okafor", email: "bob@example.com" },
};

// ---------- process and HTTP helpers ----------

let serverProc = null;
let workDir = null;

function cleanup() {
  if (serverProc && serverProc.exitCode === null) serverProc.kill("SIGTERM");
  serverProc = null;
  if (workDir) rmSync(workDir, { recursive: true, force: true });
  workDir = null;
}
process.on("exit", cleanup);
process.on("SIGINT", () => process.exit(130));
process.on("SIGTERM", () => process.exit(143));

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const hideTokens = (s) => s.replace(/ngp_[0-9a-f]+/g, "ngp_***");

function freePort() {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.on("error", reject);
    srv.listen(0, "127.0.0.1", () => {
      const { port } = srv.address();
      srv.close(() => resolve(port));
    });
  });
}

function git(args, { cwd, env = process.env, allowFail = false } = {}) {
  const r = spawnSync("git", args, { cwd, env, encoding: "utf8" });
  const out = `${r.stdout ?? ""}${r.stderr ?? ""}`;
  if (r.status !== 0 && !allowFail) {
    throw new Error(`git ${args.map(hideTokens).join(" ")} failed (exit ${r.status})\n${hideTokens(out)}`);
  }
  return { status: r.status, out };
}

// Git dates in the raw "<epoch> +0000" form, so commits spread over recent weeks.
function rawDate(daysAgo) {
  return `${Math.floor((Date.now() - daysAgo * 86400e3) / 1000)} +0000`;
}

function identityEnv(who, date) {
  const u = USERS[who];
  const env = {
    ...process.env,
    GIT_AUTHOR_NAME: u.name,
    GIT_AUTHOR_EMAIL: u.email,
    GIT_COMMITTER_NAME: u.name,
    GIT_COMMITTER_EMAIL: u.email,
  };
  if (date) {
    env.GIT_AUTHOR_DATE = date;
    env.GIT_COMMITTER_DATE = date;
  }
  return env;
}

function writeFiles(dir, files) {
  for (const [rel, content] of Object.entries(files)) {
    const file = path.join(dir, rel);
    mkdirSync(path.dirname(file), { recursive: true });
    writeFileSync(file, content);
  }
}

function commit(dir, who, message, daysAgo) {
  git(["add", "-A"], { cwd: dir });
  const date = rawDate(daysAgo);
  git(["-c", "commit.gpgsign=false", "commit", "-q", "-m", message], { cwd: dir, env: identityEnv(who, date) });
  return git(["rev-parse", "HEAD"], { cwd: dir }).out.trim();
}

// ---------- fixture content: the "aurora" weather-station project ----------

const README = `# Aurora

Aurora is a small weather-station dashboard. A Go service keeps the latest
sensor readings in memory and serves them as JSON, and a TypeScript page draws
them as charts.

## Features

- Live temperature and humidity charts
- JSON API for recent readings (\`/api/readings\`)
- Single static binary, no external database

## Getting started

\`\`\`sh
go run ./cmd/aurora -addr :8080
\`\`\`

Then open <http://localhost:8080>.

## Layout

| Path | What is there |
|------|---------------|
| \`cmd/aurora\` | Entry point |
| \`internal/server\` | HTTP handlers |
| \`internal/store\` | In-memory ring buffer |
| \`web/src\` | TypeScript chart code |
| \`web/static\` | HTML and CSS |
| \`docs\` | API notes |

## License

MIT
`;

const GO_MOD = `module github.com/alice/aurora

go 1.24
`;

const GITIGNORE = `/bin/
/web/dist/
node_modules/
`;

const MAIN_V1 = `// Command aurora serves the weather dashboard.
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/alice/aurora/internal/server"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	srv := server.New()
	log.Printf("aurora listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, srv.Handler()))
}
`;

const SERVER_V1 = `// Package server exposes the dashboard and its JSON API.
package server

import (
	"encoding/json"
	"net/http"
)

// Server routes HTTP requests for Aurora.
type Server struct {
	mux *http.ServeMux
}

// New returns a server with the health endpoint registered.
func New() *Server {
	s := &Server{mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /healthz", s.health)
	return s
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
`;

// Shipped first with a bug: before the buffer fills, Snapshot starts at the
// next write slot and returns empty readings. Fixed later in the history.
const RING_FIXED = `// Package store keeps the most recent sensor readings in memory.
package store

import (
	"sync"
	"time"
)

// Reading is one sample from a weather station.
type Reading struct {
	At          time.Time \`json:"at"\`
	Station     string    \`json:"station"\`
	Temperature float64   \`json:"temperature_c"\`
	Humidity    float64   \`json:"humidity_pct"\`
}

// Ring is a fixed-size, concurrency-safe buffer of readings.
// When it is full, the oldest reading is overwritten.
type Ring struct {
	mu    sync.RWMutex
	buf   []Reading
	next  int
	count int
}

// NewRing returns a buffer that holds up to size readings.
func NewRing(size int) *Ring {
	return &Ring{buf: make([]Reading, size)}
}

// Add appends a reading, overwriting the oldest one when full.
func (r *Ring) Add(v Reading) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = v
	r.next = (r.next + 1) % len(r.buf)
	if r.count < len(r.buf) {
		r.count++
	}
}

// Snapshot returns the stored readings, oldest first.
func (r *Ring) Snapshot() []Reading {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Reading, 0, r.count)
	start := (r.next - r.count + len(r.buf)) % len(r.buf)
	for i := 0; i < r.count; i++ {
		out = append(out, r.buf[(start+i)%len(r.buf)])
	}
	return out
}
`;
const RING_BUGGY = RING_FIXED.replace("start := (r.next - r.count + len(r.buf)) % len(r.buf)", "start := r.next");

const RING_TEST = `package store

import "testing"

func TestSnapshotBeforeFull(t *testing.T) {
	r := NewRing(4)
	r.Add(Reading{Station: "a", Temperature: 1})
	r.Add(Reading{Station: "a", Temperature: 2})
	got := r.Snapshot()
	if len(got) != 2 || got[0].Temperature != 1 || got[1].Temperature != 2 {
		t.Fatalf("snapshot = %+v, want the two readings in order", got)
	}
}

func TestSnapshotWraps(t *testing.T) {
	r := NewRing(3)
	for i := 1; i <= 5; i++ {
		r.Add(Reading{Temperature: float64(i)})
	}
	got := r.Snapshot()
	if len(got) != 3 || got[0].Temperature != 3 || got[2].Temperature != 5 {
		t.Fatalf("snapshot = %+v, want 3, 4, 5", got)
	}
}
`;

const MAIN_V2 = MAIN_V1.replace(
  "\t\"github.com/alice/aurora/internal/server\"\n",
  "\t\"github.com/alice/aurora/internal/server\"\n\t\"github.com/alice/aurora/internal/store\"\n",
).replace("\tsrv := server.New()\n", "\tring := store.NewRing(512)\n\tsrv := server.New(ring)\n");

const SERVER_V2 = `// Package server exposes the dashboard and its JSON API.
package server

import (
	"encoding/json"
	"net/http"

	"github.com/alice/aurora/internal/store"
)

// Server routes HTTP requests for Aurora.
type Server struct {
	ring *store.Ring
	mux  *http.ServeMux
}

// New returns a server that reads from ring.
func New(ring *store.Ring) *Server {
	s := &Server{ring: ring, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /healthz", s.health)
	s.mux.HandleFunc("GET /api/readings", s.listReadings)
	s.mux.Handle("GET /", http.FileServer(http.Dir("web/static")))
	return s
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
`;

const API_GO = `package server

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// listReadings writes the most recent readings, oldest first.
// The limit query parameter caps how many are returned.
func (s *Server) listReadings(w http.ResponseWriter, r *http.Request) {
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit <= 0 {
		limit = 100
	}
	all := s.ring.Snapshot()
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"readings": all})
}
`;

const TSCONFIG = `{
  "compilerOptions": {
    "target": "ES2022",
    "module": "ESNext",
    "strict": true,
    "lib": ["ES2022", "DOM"],
    "outDir": "dist"
  },
  "include": ["src"]
}
`;

const CHART_TS = `export interface Reading {
  at: string;
  station: string;
  temperature_c: number;
  humidity_pct: number;
}

export type SeriesKey = "temperature_c" | "humidity_pct";

export interface ChartOptions {
  width: number;
  height: number;
}

/** Maps values to SVG path data, scaled to the chart box. */
export function linePath(values: number[], opts: ChartOptions): string {
  if (values.length === 0) {
    return "";
  }
  const min = Math.min(...values);
  const max = Math.max(...values);
  const span = max - min || 1;
  const step = opts.width / Math.max(values.length - 1, 1);
  return values
    .map(function (v, i) {
      const x = (i * step).toFixed(1);
      const y = (opts.height - ((v - min) / span) * opts.height).toFixed(1);
      return (i === 0 ? "M" : "L") + x + "," + y;
    })
    .join(" ");
}

export function render(svg: SVGSVGElement, readings: Reading[], key: SeriesKey): void {
  const path = svg.querySelector<SVGPathElement>("path.series");
  if (path === null) {
    return;
  }
  const values = readings.map(function (r) {
    return r[key];
  });
  path.setAttribute("d", linePath(values, { width: 640, height: 200 }));
}
`;

const MAIN_TS = `import { render, type Reading } from "./chart";

async function refresh(svg: SVGSVGElement): Promise<void> {
  const res = await fetch("/api/readings?limit=120");
  if (!res.ok) {
    throw new Error("readings: HTTP " + res.status);
  }
  const body = (await res.json()) as { readings: Reading[] };
  render(svg, body.readings, "temperature_c");
}

const svg = document.querySelector<SVGSVGElement>("#temp-chart");
if (svg !== null) {
  refresh(svg).catch(console.error);
  window.setInterval(function () {
    refresh(svg).catch(console.error);
  }, 30000);
}
`;

const INDEX_HTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Aurora</title>
  <link rel="stylesheet" href="style.css">
</head>
<body>
  <header>
    <h1>Aurora</h1>
    <span class="station">Station: rooftop-01</span>
  </header>
  <main>
    <section class="card">
      <h2>Temperature</h2>
      <svg id="temp-chart" viewBox="0 0 640 200" role="img" aria-label="Temperature over time">
        <path class="series" fill="none" stroke="currentColor" stroke-width="2"></path>
      </svg>
    </section>
  </main>
  <script type="module" src="main.js"></script>
</body>
</html>
`;

const STYLE_BASE = `* { box-sizing: border-box; }
body {
  margin: 0;
  font: 16px/1.5 system-ui, sans-serif;
  background: #f7f7f5;
  color: #1d1f21;
}
header {
  display: flex;
  align-items: baseline;
  gap: 16px;
  padding: 16px 24px;
  border-bottom: 1px solid #e2e2de;
}
header h1 { margin: 0; font-size: 20px; }
.station { color: #6b6f76; font-size: 14px; }
main { display: grid; gap: 16px; max-width: 960px; padding: 24px; }
.card { background: #ffffff; border: 1px solid #e2e2de; border-radius: 8px; padding: 16px; }
.card h2 { margin: 0 0 8px; font-size: 15px; font-weight: 600; color: #6b6f76; }
svg { width: 100%; height: auto; color: #2d6cdf; }
`;

// Bob's first commit moves the palette into custom properties.
const STYLE_VARS = `:root {
  --bg: #f7f7f5;
  --fg: #1d1f21;
  --muted: #6b6f76;
  --border: #e2e2de;
  --card: #ffffff;
  --accent: #2d6cdf;
}
* { box-sizing: border-box; }
body {
  margin: 0;
  font: 16px/1.5 system-ui, sans-serif;
  background: var(--bg);
  color: var(--fg);
}
header {
  display: flex;
  align-items: baseline;
  gap: 16px;
  padding: 16px 24px;
  border-bottom: 1px solid var(--border);
}
header h1 { margin: 0; font-size: 20px; }
.station { color: var(--muted); font-size: 14px; }
main { display: grid; gap: 16px; max-width: 960px; padding: 24px; }
.card { background: var(--card); border: 1px solid var(--border); border-radius: 8px; padding: 16px; }
.card h2 { margin: 0 0 8px; font-size: 15px; font-weight: 600; color: var(--muted); }
svg { width: 100%; height: auto; color: var(--accent); }
`;

// Bob's second commit adds the dark palette; v2 lightens the muted text.
const STYLE_DARK = STYLE_VARS + `
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #101214;
    --fg: #e6e6e3;
    --muted: #9a9ea6;
    --border: #2a2e33;
    --card: #17191c;
    --accent: #7aa7ff;
  }
}
`;
const STYLE_DARK_V2 = STYLE_DARK.replace("--muted: #9a9ea6;", "--muted: #aab0b8;");

// Alice's history on main (oldest first). `key` names the commit for later use.
const AURORA_MAIN = [
  { key: "init", days: 24, msg: "Initial commit: project layout and README", files: { "README.md": README, "go.mod": GO_MOD, ".gitignore": GITIGNORE } },
  { days: 22, msg: "Add HTTP server with health endpoint", files: { "cmd/aurora/main.go": MAIN_V1, "internal/server/server.go": SERVER_V1 } },
  { days: 19, msg: "Keep recent readings in a ring buffer", files: { "internal/store/ring.go": RING_BUGGY } },
  { days: 16, msg: "Serve readings over /api/readings", files: { "cmd/aurora/main.go": MAIN_V2, "internal/server/server.go": SERVER_V2, "internal/server/api.go": API_GO } },
  { days: 13, msg: "Add TypeScript chart module", files: { "web/tsconfig.json": TSCONFIG, "web/src/chart.ts": CHART_TS, "web/src/main.ts": MAIN_TS } },
  { days: 10, msg: "Add dashboard page and styles", files: { "web/static/index.html": INDEX_HTML, "web/static/style.css": STYLE_BASE } },
  { days: 7, msg: "Document the readings API", files: { "docs/api.md": `# Readings API

Aurora serves the most recent sensor readings as JSON. Nothing is written to
disk, so the history resets when the process restarts.

## Endpoints

| Method | Path | Description |
|--------|------|-------------|
| \`GET\` | \`/healthz\` | Liveness check |
| \`GET\` | \`/api/readings\` | Recent readings, oldest first |

## \`GET /api/readings\`

Query parameters:

- \`limit\` (optional, default \`100\`): maximum number of readings to return

Example:

\`\`\`sh
curl -s 'http://localhost:8080/api/readings?limit=2'
\`\`\`

Response:

\`\`\`json
{
  "readings": [
    {
      "at": "2026-10-20T09:14:00Z",
      "station": "rooftop-01",
      "temperature_c": 11.4,
      "humidity_pct": 72.5
    }
  ]
}
\`\`\`

## Notes

- Readings come back in the order they arrived.
- The ring buffer holds 512 readings. Older samples are dropped.
` } },
  { key: "fix", days: 4, msg: "Fix Snapshot returning empty readings before the buffer fills", files: { "internal/store/ring.go": RING_FIXED, "internal/store/ring_test.go": RING_TEST } },
];

// Alice's branch: two commits ahead of main.
const AURORA_SEARCH = [
  { days: 3, msg: "Add station and time filters to the store", files: { "internal/store/filter.go": `package store

import "time"

// Filter selects readings from one station recorded at or after since.
// An empty station matches every station; a zero since matches all times.
func Filter(readings []Reading, station string, since time.Time) []Reading {
	out := make([]Reading, 0, len(readings))
	for _, r := range readings {
		if station != "" && r.Station != station {
			continue
		}
		if !since.IsZero() && r.At.Before(since) {
			continue
		}
		out = append(out, r)
	}
	return out
}
` } },
  { days: 2, msg: "Expose search at /api/search", files: {
    "internal/server/search.go": `package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/alice/aurora/internal/store"
)

// search returns readings filtered by ?station= and ?since= (RFC 3339).
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var since time.Time
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			http.Error(w, "since must be RFC 3339", http.StatusBadRequest)
			return
		}
		since = t
	}
	hits := store.Filter(s.ring.Snapshot(), q.Get("station"), since)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"readings": hits})
}
`,
    "internal/server/server.go": SERVER_V2.replace(
      's.mux.HandleFunc("GET /api/readings", s.listReadings)',
      's.mux.HandleFunc("GET /api/readings", s.listReadings)\n\ts.mux.HandleFunc("GET /api/search", s.search)',
    ),
  } },
];

const LANTERN_README = `# Lantern

Lantern turns a folder of Markdown notes into a static site.

## Usage

\`\`\`sh
lantern build ./notes ./site
\`\`\`

Each \`.md\` file becomes one page. Folders become sections in the index.
`;

// ---------- seeding ----------

async function seed({ base, work, port }) {
  const keys = { alice: path.join(work, "alice.json"), bob: path.join(work, "bob.json") };
  const dc = (...a) => execFileSync(process.execPath, [DEVICE_CLIENT, ...a], { encoding: "utf8" }).trim();

  dc("register", base, "alice", keys.alice);
  dc("register", base, "bob", keys.bob);
  const sessionCookie = { alice: dc("login", base, keys.alice), bob: dc("login", base, keys.bob) };
  const token = {
    alice: dc("token", base, keys.alice, "laptop", "write"),
    bob: dc("token", base, keys.bob, "laptop", "write"),
  };
  dc("token", base, keys.alice, "ci-readonly", "read");

  // Form posts need the CSRF token from a page the session can see.
  async function post(who, url, fields) {
    const page = await (await fetch(`${base}/-/device`, { headers: { Cookie: sessionCookie[who] } })).text();
    const csrf = (page.match(/name="csrf" value="([0-9a-f]+)"/) || [])[1];
    if (!csrf) throw new Error("no CSRF token on /-/device");
    const res = await fetch(`${base}${url}`, {
      method: "POST",
      headers: { Cookie: sessionCookie[who], "Content-Type": "application/x-www-form-urlencoded" },
      body: new URLSearchParams({ csrf, ...fields }),
      redirect: "manual",
    });
    if (res.status !== 302 && res.status !== 303) throw new Error(`POST ${url} returned ${res.status}`);
    return res.headers.get("location");
  }
  const authed = (who, repoPath) => base.replace("://", `://x:${token[who]}@`) + `/${repoPath}.git`;

  // Repositories. Owners are the signed-in account that creates them.
  await post("alice", "/-/new", { name: "aurora", description: "Weather-station dashboard in Go and TypeScript" });
  await post("bob", "/-/new", { name: "lantern", description: "Markdown notes to a static site" });
  await post("alice", "/-/new", { name: "sandbox", description: "" });

  // alice/aurora: history on main, then a two-commit branch.
  const aDir = path.join(work, "aurora");
  mkdirSync(aDir, { recursive: true });
  git(["init", "-q", "-b", "main"], { cwd: aDir });
  const sha = {};
  for (const step of AURORA_MAIN) {
    writeFiles(aDir, step.files);
    const id = commit(aDir, "alice", step.msg, step.days);
    if (step.key) sha[step.key] = id;
  }
  git(["checkout", "-q", "-b", "feature/search"], { cwd: aDir });
  for (const step of AURORA_SEARCH) {
    writeFiles(aDir, step.files);
    commit(aDir, "alice", step.msg, step.days);
  }
  git(["checkout", "-q", "main"], { cwd: aDir });
  git(["tag", "-a", "v0.1.0", "-m", "First tagged release", "main~2"], { cwd: aDir, env: identityEnv("alice") });
  git(["tag", "v0.2.0", "main"], { cwd: aDir });
  git(["push", "-q", authed("alice", "alice/aurora"), "main", "feature/search", "--tags"], { cwd: aDir });

  // bob/lantern: a small second project.
  const lDir = path.join(work, "lantern");
  mkdirSync(lDir, { recursive: true });
  git(["init", "-q", "-b", "main"], { cwd: lDir });
  writeFiles(lDir, { "README.md": LANTERN_README });
  commit(lDir, "bob", "Initial commit", 12);
  writeFiles(lDir, { "notes/hello.md": "# Hello\n\nFirst note. Lantern renders it to `site/hello.html`.\n" });
  commit(lDir, "bob", "Add sample notes", 9);
  git(["push", "-q", authed("bob", "bob/lantern"), "main"], { cwd: lDir });

  // bob's AGit merge request: two commits, then a second version after amend.
  const bDir = path.join(work, "aurora-bob");
  git(["clone", "-q", `${base}/alice/aurora.git`, bDir], {});
  git(["checkout", "-q", "-b", "dark-theme", "origin/main"], { cwd: bDir });
  writeFiles(bDir, { "web/static/style.css": STYLE_VARS });
  commit(bDir, "bob", "Move dashboard colors into CSS custom properties", 6);
  writeFiles(bDir, { "web/static/style.css": STYLE_DARK });
  commit(bDir, "bob", "Add dark theme that follows the system setting", 5);
  const agitOut = git(
    ["push", authed("bob", "alice/aurora"), "HEAD:refs/for/main", "-o", "title=Add dark theme for the dashboard",
      "-o", "description=Follows prefers-color-scheme. Colors move into custom properties first, so the theme is one block.",
      "-o", "topic=dark-theme"],
    { cwd: bDir },
  ).out;
  const bobMR = Number((agitOut.match(/Created merge request !(\d+)/) || [])[1]);
  if (!bobMR) throw new Error("AGit push did not open a merge request:\n" + agitOut);

  writeFiles(bDir, { "web/static/style.css": STYLE_DARK_V2 });
  git(["add", "-A"], { cwd: bDir });
  git(["commit", "-q", "--amend", "-m", "Add dark theme that follows the system setting"], { cwd: bDir, env: identityEnv("bob") });
  const v2 = git(["push", authed("bob", "alice/aurora"), "HEAD:refs/for/main", "-o", "topic=dark-theme"], { cwd: bDir }).out;
  if (!v2.includes("version 2")) throw new Error("second AGit push did not record version 2:\n" + v2);

  // alice's branch merge request, opened through the web form.
  const branchMR = Number(((await post("alice", "/alice/aurora/-/merge_requests/new", {
    source: "feature/search",
    target: "main",
    title: "Add search by station and time",
    description: "Adds `store.Filter` and a `/api/search` endpoint that filters readings by station and start time.\n\nThe ring buffer stays the source of truth. Search only reads a snapshot.",
  })) || "").split("?")[0].split("/").pop());
  if (!branchMR) throw new Error("branch merge request was not created");

  await post("alice", `/alice/aurora/-/merge_requests/${bobMR}/comment`, {
    body: "The version split makes the diff easy to review. Could you check the muted text on the card background? It looked borderline in the dark preview.",
  });
  await post("bob", `/alice/aurora/-/merge_requests/${bobMR}/comment`, {
    body: "Pushed v2 with a lighter muted color. The Versions tab shows the range-diff against v1.",
  });
  await post("bob", `/alice/aurora/-/merge_requests/${branchMR}/comment`, {
    body: "Read through `Filter` and the handler. One nit: `since` should say it is inclusive in `docs/api.md`.",
  });

  // Issues. Numbers are per repository, in creation order.
  await post("bob", "/alice/aurora/-/issues/new", {
    title: "Chart x-axis labels overlap on narrow screens",
    body: `On phones the time labels under **Temperature** collide with each other.

## Steps to reproduce

1. Open the dashboard at 390 px wide.
2. Look at the x-axis under the chart.

Expected: labels are thinned out or rotated. Actual: they overlap.

## Notes

Below 480 px, showing every fourth label should be enough.`,
  });
  await post("alice", "/alice/aurora/-/issues/1/comment", {
    body: "Reproduced at phone width. I will try thinning the labels below 480 px and send a change.",
  });
  await post("bob", "/alice/aurora/-/issues/1/comment", {
    body: "Thanks! Happy to test on my phone once there is a build.",
  });

  await post("alice", "/alice/aurora/-/issues/new", {
    title: "Add CSV export for readings",
    body: `Users want to pull a day of readings into a spreadsheet.

## Proposal

| Endpoint | Output | Notes |
|----------|--------|-------|
| \`GET /api/readings.csv\` | CSV | Same filters as \`/api/readings\` |
| \`GET /api/readings\` | JSON | Unchanged |

## Tasks

- [x] Decide the column order
- [ ] Stream rows instead of buffering them
- [ ] Set \`Content-Disposition\` so browsers save the file`,
  });
  await post("bob", "/alice/aurora/-/issues/2/comment", {
    body: "Column order \`at, station, temperature_c, humidity_pct\` would match the JSON. +1 to streaming.",
  });

  await post("bob", "/alice/aurora/-/issues/new", {
    title: "Snapshot returns empty readings before the buffer fills",
    body: `Right after startup, \`GET /api/readings\` returns entries with \`temperature_c: 0\` until the buffer has 512 samples. The chart drops from a flat zero line to real values.

\`Ring.Snapshot\` starts at \`r.next\` when the buffer is not full, so it reads the unused slots first.`,
  });
  await post("bob", "/alice/aurora/-/issues/3/comment", {
    body: "Repro: start the server, call the endpoint, and count the zero entries.",
  });
  await post("alice", "/alice/aurora/-/issues/3/comment", {
    body: `Fixed in ${sha.fix.slice(0, 7)} with a regression test. Closing.`,
    close: "1",
  });

  return { sessionCookie, fixSha: sha.fix, bobMR, branchMR };
}

// ---------- screenshots ----------

function shotList({ fixSha, bobMR }) {
  const repo = "/alice/aurora";
  const mr = `${repo}/-/merge_requests/${bobMR}`;
  const ring = `${repo}/-/blob/main/internal/store/ring.go`;
  return [
    { name: "home-signed-out", path: "/" },
    { name: "explore", path: "/explore" },
    { name: "signin", path: "/-/signin" },
    { name: "device-tokens", path: "/-/device", as: "alice" },
    { name: "new-project", path: "/-/new", as: "alice" },
    { name: "home-signed-in", path: "/", as: "alice" },
    { name: "user-alice", path: "/alice" },
    { name: "repo-overview", path: repo, as: "alice" },
    { name: "repo-empty", path: "/alice/sandbox", as: "alice" },
    { name: "tree-subdir", path: `${repo}/-/tree/main/internal/server`, as: "alice" },
    { name: "blob-go", path: ring, as: "alice" },
    { name: "blob-markdown", path: `${repo}/-/blob/main/docs/api.md`, as: "alice" },
    { name: "commits", path: `${repo}/-/commits/main`, as: "alice" },
    { name: "commit-detail", path: `${repo}/-/commit/${fixSha}`, as: "alice" },
    { name: "branches", path: `${repo}/-/branches`, as: "alice" },
    { name: "tags", path: `${repo}/-/tags`, as: "alice" },
    { name: "push-log", path: `${repo}/-/pushes`, as: "alice" },
    { name: "issues", path: `${repo}/-/issues?state=all`, as: "alice" },
    { name: "issue-detail", path: `${repo}/-/issues/1`, as: "alice" },
    { name: "issue-closed", path: `${repo}/-/issues/3`, as: "alice" },
    { name: "issue-new", path: `${repo}/-/issues/new`, as: "alice" },
    { name: "mr-list", path: `${repo}/-/merge_requests?state=all`, as: "alice" },
    { name: "mr-new", path: `${repo}/-/merge_requests/new?source=feature/search`, as: "alice" },
    { name: "mr-overview", path: mr, as: "alice" },
    { name: "mr-commits", path: `${mr}?tab=commits`, as: "alice" },
    { name: "mr-changes", path: `${mr}?tab=changes`, as: "alice" },
    { name: "mr-versions", path: `${mr}?tab=versions`, as: "alice" },
    { name: "mr-interdiff", path: `${mr}?tab=changes&from=1&to=2`, as: "alice" },
    { name: "error-404", path: "/alice/no-such-project", status: 404 },
    { name: "mobile-home", path: "/", as: "alice", view: "mobile" },
    { name: "mobile-repo-overview", path: repo, as: "alice", view: "mobile" },
    { name: "mobile-mr-overview", path: mr, as: "alice", view: "mobile" },
    { name: "dark-repo-overview", path: repo, as: "alice", scheme: "dark" },
    { name: "dark-blob-go", path: ring, as: "alice", scheme: "dark" },
    { name: "dark-mr-changes", path: `${mr}?tab=changes`, as: "alice", scheme: "dark" },
  ];
}

async function capture(browser, base, sessionCookie, shots) {
  const contexts = new Map();
  async function contextFor(as, view, scheme) {
    const key = [as ?? "-", view, scheme].join("|");
    if (!contexts.has(key)) {
      const viewport = view === "mobile" ? { width: 390, height: 844 } : { width: 1440, height: 900 };
      const ctx = await browser.newContext({ viewport, colorScheme: scheme, deviceScaleFactor: 1 });
      if (as) {
        const value = sessionCookie[as].slice(sessionCookie[as].indexOf("=") + 1);
        await ctx.addCookies([{ name: "ng_session", value, url: base }]);
      }
      contexts.set(key, ctx);
    }
    return contexts.get(key);
  }

  const results = [];
  for (const shot of shots) {
    const ctx = await contextFor(shot.as, shot.view ?? "desktop", shot.scheme ?? "light");
    const page = await ctx.newPage();
    const notes = [];
    page.on("pageerror", (err) => notes.push(`js: ${err.message}`));
    const resp = await page.goto(base + shot.path, { waitUntil: "load" });
    await page.waitForLoadState("networkidle").catch(() => {});
    const status = resp ? resp.status() : 0;
    const text = await page.locator("body").innerText().catch(() => "");
    const file = path.join(OUT_DIR, `${shot.name}.png`);
    await page.screenshot({ path: file, fullPage: true });
    await page.close();

    const expected = shot.status ?? 200;
    const problems = [];
    if (status >= 500) problems.push(`HTTP ${status}`);
    if (status !== expected) problems.push(`HTTP ${status}, expected ${expected}`);
    if (/template error|missing template/.test(text)) problems.push("template error");
    results.push({ name: shot.name, status, file, problems, notes });
  }
  for (const ctx of contexts.values()) await ctx.close();
  return results;
}

// ---------- main ----------

async function main() {
  workDir = mkdtempSync(path.join(os.tmpdir(), "newgit-shots-"));
  const dataDir = path.join(workDir, "data");
  const port = await freePort();
  const base = `http://localhost:${port}`;

  console.log(`building ${BIN}`);
  execFileSync("go", ["build", "-o", BIN, "./cmd/newgit"], { cwd: ROOT, stdio: "inherit" });

  const logPath = path.join(workDir, "server.log");
  const logFd = openSync(logPath, "w");
  serverProc = spawn(BIN, ["-addr", `127.0.0.1:${port}`, "-data", dataDir, "-base-url", base], {
    stdio: ["ignore", logFd, logFd],
  });
  let up = false;
  for (let i = 0; i < 100 && !up; i++) {
    up = await fetch(`${base}/-/health`).then((r) => r.ok, () => false);
    if (!up) await sleep(100);
  }
  if (!up) throw new Error(`server did not start, see ${logPath}`);
  console.log(`server up at ${base} (data ${dataDir})`);

  console.log("seeding demo data");
  const seeded = await seed({ base, work: workDir, port });

  mkdirSync(OUT_DIR, { recursive: true });
  const browser = await chromium.launch();
  let results;
  try {
    results = await capture(browser, base, seeded.sessionCookie, shotList(seeded));
  } finally {
    await browser.close();
  }

  console.log("");
  let failed = 0;
  for (const r of results) {
    const size = statSync(r.file).size;
    const flag = r.problems.length ? `FAIL (${r.problems.join("; ")})` : "ok";
    if (r.problems.length) failed++;
    console.log(`  ${flag === "ok" ? "ok  " : "FAIL"}  ${r.name.padEnd(22)} ${r.status}  ${Math.round(size / 1024)} KB  ${r.file}`);
    for (const n of r.notes) console.log(`        warning: ${n}`);
  }
  console.log(`\n${results.length - failed}/${results.length} pages captured in ${OUT_DIR}`);

  if (KEEP) {
    const keyFile = path.join(workDir, "alice.json");
    console.log(`\nNewGit is running at ${base}`);
    console.log(`Sign in as alice with: node scripts/device-client.mjs login ${base} ${keyFile}`);
    console.log("Press Ctrl-C to stop. The server and its data directory are removed on exit.");
    await new Promise((resolve) => serverProc.once("exit", resolve));
  }
  return failed ? 1 : 0;
}

// The server child keeps the event loop alive, so exit explicitly after cleanup.
main().then(
  (code) => {
    cleanup();
    process.exit(code);
  },
  (err) => {
    console.error(err.stack || err);
    cleanup();
    process.exit(1);
  },
);
