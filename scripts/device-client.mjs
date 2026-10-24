#!/usr/bin/env node
// Headless NewGit device client, for scripts, tests and journey simulations.
//
// It performs the same proof-of-possession protocol as the browser
// (ECDSA P-256 via WebCrypto, signature over a one-time challenge), but keeps
// its key in a local JSON file instead of a browser's IndexedDB.
//
//   node scripts/device-client.mjs register <baseURL> <username> <keyfile>
//   node scripts/device-client.mjs login    <baseURL> <keyfile>
//   node scripts/device-client.mjs token    <baseURL> <keyfile> <name> [read|write]
//
// login prints the session cookie; token prints a new personal access token.
import { readFileSync, writeFileSync } from "node:fs";
const { subtle } = globalThis.crypto;

const b64url = (buf) => Buffer.from(buf).toString("base64url");
const msg = (purpose, nonce) => new TextEncoder().encode(`newgit-device-auth:v1|${purpose}|${nonce}`);

async function thumbprint(x, y) {
  const canon = `{"crv":"P-256","kty":"EC","x":"${x}","y":"${y}"}`;
  return b64url(await subtle.digest("SHA-256", new TextEncoder().encode(canon)));
}
async function challenge(base) {
  const r = await fetch(`${base}/-/auth/challenge`);
  return (await r.json()).nonce;
}
async function signWith(privJwk, purpose, nonce) {
  const key = await subtle.importKey("jwk", privJwk, { name: "ECDSA", namedCurve: "P-256" }, false, ["sign"]);
  return b64url(await subtle.sign({ name: "ECDSA", hash: "SHA-256" }, key, msg(purpose, nonce)));
}
async function post(url, body, cookie) {
  const r = await fetch(url, { method: "POST", headers: { "Content-Type": "application/json", ...(cookie ? { Cookie: cookie } : {}) }, body: JSON.stringify(body), redirect: "manual" });
  const setCookie = r.headers.get("set-cookie") || "";
  const text = await r.text();
  let json = {};
  try { json = JSON.parse(text); } catch {}
  return { status: r.status, json, cookie: setCookie.split(";")[0] };
}

async function register(base, username, keyfile) {
  const kp = await subtle.generateKey({ name: "ECDSA", namedCurve: "P-256" }, true, ["sign", "verify"]);
  const priv = await subtle.exportKey("jwk", kp.privateKey);
  const id = await thumbprint(priv.x, priv.y);
  const nonce = await challenge(base);
  const sig = await signWith(priv, "register", nonce);
  const res = await post(`${base}/-/auth/register`, { username, x: priv.x, y: priv.y, nonce, sig });
  if (res.status !== 200) throw new Error(`register failed (${res.status}): ${res.json.error}`);
  writeFileSync(keyfile, JSON.stringify({ id, username, priv }, null, 2), { mode: 0o600 });
  return res.cookie;
}

async function login(base, keyfile) {
  const k = JSON.parse(readFileSync(keyfile, "utf8"));
  const nonce = await challenge(base);
  const sig = await signWith(k.priv, "login", nonce);
  const res = await post(`${base}/-/auth/login`, { id: k.id, nonce, sig });
  if (res.status !== 200) throw new Error(`login failed (${res.status}): ${res.json.error}`);
  return res.cookie;
}

async function token(base, keyfile, name, scope) {
  const cookie = await login(base, keyfile);
  const page = await (await fetch(`${base}/-/device`, { headers: { Cookie: cookie } })).text();
  const csrf = (page.match(/name="csrf" value="([0-9a-f]+)"/) || [])[1];
  if (!csrf) throw new Error("could not find CSRF token on the device page");
  const form = new URLSearchParams({ csrf, name, scope: scope || "write" });
  const r = await fetch(`${base}/-/device/tokens`, { method: "POST", headers: { Cookie: cookie, "Content-Type": "application/x-www-form-urlencoded" }, body: form, redirect: "manual" });
  const tokCookie = (r.headers.get("set-cookie") || "").split(";")[0];
  const shown = await (await fetch(`${base}/-/device`, { headers: { Cookie: `${cookie}; ${tokCookie}` } })).text();
  const tok = (shown.match(/(ngp_[0-9a-f]+)/) || [])[1];
  if (!tok) throw new Error("token was not shown");
  return tok;
}

const [cmd, base, ...rest] = process.argv.slice(2);
try {
  if (cmd === "register") console.log(await register(base, rest[0], rest[1]));
  else if (cmd === "login") console.log(await login(base, rest[0]));
  else if (cmd === "token") console.log(await token(base, rest[0], rest[1], rest[2]));
  else { console.error("usage: register|login|token ..."); process.exit(2); }
} catch (e) {
  console.error(e.message);
  process.exit(1);
}
