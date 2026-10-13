// Runs the REAL web/app.js against index.html's real element ids with a fake
// server, driving the same path a user takes: drop clips, press Create, upload,
// poll, show the result. It exists because a missing element once froze every
// upload at 0% while all server-side tests passed.
import fs from "node:fs";
import vm from "node:vm";
import path from "node:path";
import { fileURLToPath } from "node:url";

const dir = path.dirname(fileURLToPath(import.meta.url));
const html = fs.readFileSync(path.join(dir, "index.html"), "utf8");
const ids = new Set([...html.matchAll(/\bid="([A-Za-z0-9_-]+)"/g)].map((m) => m[1]));

const elements = new Map();
const toasts = [];
function makeEl(id) {
  const listeners = {};
  const attrs = {};
  const el = {
    id, hidden: false, textContent: "", innerHTML: "", value: "", files: [], disabled: false, title: "",
    style: {}, dataset: {}, children: [], open: false,
    classList: { add() {}, remove() {}, toggle() {}, contains: () => false },
    addEventListener(t, f) { (listeners[t] ||= []).push(f); },
    removeEventListener() {},
    setAttribute(k, v) { attrs[k] = v; }, getAttribute: (k) => attrs[k],
    appendChild(c) {
      el.children.push(c);
      if (el.id === "toasts" && String(c.className).includes("err")) toasts.push(c.textContent);
      return c;
    },
    load() {}, play: async () => {}, pause() {},
    remove() {}, focus() {}, click() { for (const f of listeners.click || []) f({ target: el, preventDefault() {}, stopPropagation() {} }); },
    querySelector: () => makeEl("_q"), querySelectorAll: () => [],
    showModal() { el.open = true; }, close() { el.open = false; },
    closest: () => el, scrollIntoView() {},
    __listeners: listeners,
  };
  return el;
}
function getEl(id) {
  if (!ids.has(id)) return null; // exactly what a real browser does
  if (!elements.has(id)) elements.set(id, makeEl(id));
  return elements.get(id);
}
// Initial hidden state comes from the page itself, as in a real browser.
for (const m of html.matchAll(/<[^>]*\bid="([A-Za-z0-9_-]+)"[^>]*>/g)) {
  if (/\shidden(\s|>|=)/.test(m[0])) getEl(m[1]).hidden = true;
}

const requests = [];
const errors = [];
let signedIn = false;
let projectStatus = "draft";
let projectMedia = [];
let projectRev = 0;
let polls = 0;
const edits = [];
const settings = { preset: "9:16", duration: 30 };

async function fakeFetch(url, opts = {}) {
  const u = String(url);
  const method = (opts.method || "GET").toUpperCase();
  requests.push(`${method} ${u}`);
  const json = (obj, status = 200) => ({ ok: status < 300, status, json: async () => obj, arrayBuffer: async () => new ArrayBuffer(0) });
  const body = () => (typeof opts.body === "string" ? JSON.parse(opts.body) : {});
  const project = () => ({
    id: "p".repeat(32), name: "Untitled project", status: projectStatus, stage: projectStatus === "done" ? "Done" : "Analyzing clips (1 of 2)",
    progress: projectStatus === "done" ? 1 : 0.4, revision: projectRev, planner: "rule-based", can_undo: false, settings,
    media: projectMedia, notes: [],
    edl: projectRev ? { cuts: [{ clip: "/x/m1.mp4", start: 0, end: 2 }, { clip: "/x/m2.mp4", start: 0, end: 3 }] } : null,
  });

  if (u.endsWith("/api/health")) return json({ ok: true, max_clips: 200, max_upload_mb: 100000, ffmpeg: true, version: "dev", extra: {} });
  if (u.endsWith("/api/auth/me")) return json({ required: true, user: signedIn ? { id: 1, email: "a@b.co", name: "A" } : null });
  if (u.endsWith("/api/auth/login") || u.endsWith("/api/auth/signup")) {
    const b = body();
    if (b.password !== "correct-horse") return json({ error: "that email and password don't match" }, 401);
    signedIn = true;
    return json({ user: { id: 1, email: b.email, name: "A" } }, 200);
  }
  if (u.includes("/api/update")) return json({ enabled: false });
  if (!signedIn && u.startsWith("/api/") && !u.includes("/auth/")) return json({ error: "sign in required" }, 401);
  if (u.endsWith("/api/verify")) return json({ ok: true });
  if (u.endsWith("/api/projects") && method === "GET") return json({ projects: [] });
  if (u.endsWith("/api/projects") && method === "POST") return json(project(), 201);
  if (/\/api\/projects\/p+$/.test(u) && method === "GET") {
    if (projectStatus === "queued") { polls++; if (polls >= 2) { projectStatus = "done"; projectRev = 1; } }
    return json(project());
  }
  if (/\/api\/projects\/p+$/.test(u) && method === "PATCH") {
    const b = body();
    if (b.media) projectMedia = b.media.map((id) => ({ id, name: id + ".mp4", duration: 10, size: 1e6, kind: "clip" }));
    if (b.settings) Object.assign(settings, b.settings);
    return json(project());
  }
  if (u.endsWith("/edit") && method === "POST") {
    const b = body();
    if (!b.ops || !b.ops.length) return json({ error: "no changes" }, 400);
    projectRev++;
    edits.push(b.ops);
    return json({ id: "p".repeat(32) }, 202);
  }
  if (u.endsWith("/generate")) { projectStatus = "queued"; return json({ id: "p".repeat(32) }, 202); }
  if (u.endsWith("/api/uploads") && method === "POST") return json({ upload_id: "u".repeat(32), chunk_bytes: 4 * 1048576 }, 201);
  if (u.endsWith("/commit")) return json({ media: [{ id: "m1", name: "a.mp4", kind: "clip", duration: 10, size: 10485760 }, { id: "m2", name: "b.mp4", kind: "clip", duration: 6, size: 6291456 }], problems: [] }, 201);
  if (u.endsWith("/api/media")) return json({ media: [], bytes: 0 });
  if (u.endsWith("/api/ping")) return json({ ok: true });
  return json({ error: "not found: " + u }, 404);
}

class FakeXHR {
  constructor() { this.upload = {}; this.headers = {}; }
  open(m, u) { this.method = m; this.url = u; }
  setRequestHeader(k, v) { this.headers[k] = v; }
  send(blob) {
    requests.push(`${this.method} ${this.url} [${blob.size} bytes @${this.headers["X-Offset"]}]`);
    queueMicrotask(() => {
      this.upload.onprogress && this.upload.onprogress({ lengthComputable: true, loaded: blob.size, total: blob.size });
      this.status = 200;
      this.responseText = JSON.stringify({ have: blob.size });
      this.onload && this.onload();
    });
  }
}

const store = new Map();
const winListeners = {};
const location = { hostname: "localhost", _hash: "", get hash() { return this._hash; },
  set hash(v) { this._hash = v; queueMicrotask(() => (winListeners.hashchange || []).forEach((f) => f())); } };
const win = {
  SPLICE_API: "",
  addEventListener(t, f) { (winListeners[t] ||= []).push(f); },
  scrollTo() {},
  location,
};
const ctx = {
  window: win,
  document: { getElementById: getEl, createElement: () => makeEl("_c"), querySelector: () => makeEl("_q") },
  localStorage: { getItem: (k) => store.get(k) ?? null, setItem: (k, v) => store.set(k, v), removeItem: (k) => store.delete(k) },
  location, navigator: {}, crypto: globalThis.crypto, performance: globalThis.performance,
  fetch: fakeFetch, XMLHttpRequest: FakeXHR, Blob, File, FormData, URL: { createObjectURL: () => "blob:x", revokeObjectURL() {} },
  DragEvent: class {}, console, setTimeout, clearTimeout, setInterval, clearInterval, queueMicrotask,
  Promise, Date, Math, JSON, Array, Object, Map, Set, Error, String, Number, RegExp, Uint8Array, ArrayBuffer, isFinite, parseInt, parseFloat,
};
win.document = ctx.document;
ctx.self = ctx.globalThis = ctx;
process.on("unhandledRejection", (e) => errors.push("unhandled rejection: " + (e && e.stack || e)));
process.on("uncaughtException", (e) => errors.push("uncaught: " + (e && e.stack || e)));

vm.createContext(ctx);
vm.runInContext(fs.readFileSync(path.join(dir, "config.js"), "utf8"), ctx);
vm.runInContext(fs.readFileSync(path.join(dir, "app.js"), "utf8"), ctx);

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const failures = [];
const until = async (cond, what, ms = 8000) => {
  const end = Date.now() + ms;
  while (Date.now() < end && !cond() && !errors.length) await sleep(20);
  if (!cond()) failures.push("timed out waiting for: " + what);
  return cond();
};

await sleep(50);

// 1. signed out: the sign-in screen is shown, nothing else
await until(() => !getEl("viewAuth").hidden, "sign-in screen");
if (!getEl("viewHome").hidden) failures.push("home visible while signed out");

// 2. wrong password shows an error and stays put
getEl("authEmail").value = "a@b.co";
getEl("authPassword").value = "wrong";
for (const f of getEl("authForm").__listeners.submit || []) f({ preventDefault() {} });
await until(() => !getEl("authErr").hidden, "wrong-password error");
if (!/match/.test(getEl("authErr").textContent)) failures.push("unexpected auth error text: " + getEl("authErr").textContent);

// 3. correct password -> home (projects dashboard)
getEl("authPassword").value = "correct-horse";
for (const f of getEl("authForm").__listeners.submit || []) f({ preventDefault() {} });
await until(() => !getEl("viewHome").hidden, "home after sign-in");

// 4. new project -> workspace
getEl("newProject").click();
await until(() => !getEl("viewProject").hidden, "project workspace");

// 5. drop two clips (4 MB chunks -> 3 + 2 chunks), they upload, commit, attach
const big = new File([new Uint8Array(10 * 1048576)], "a.mp4", { type: "video/mp4" });
const small = new File([new Uint8Array(6 * 1048576)], "b.mp4", { type: "video/mp4" });
for (const f of getEl("drop").__listeners.drop || []) f({ preventDefault() {}, dataTransfer: { files: [big, small] } });
await until(() => projectMedia.length === 2, "both clips attached to the project");
await until(() => !getEl("go").disabled, "Create button enabled");

// 6. generate -> polls -> result view bits appear
getEl("prompt").value = "calm";
getEl("go").click();
await until(() => !getEl("timelineCard").hidden, "timeline after generation");

// 7. timeline: select the first shot, slow it down, delete another, apply once
const tl = getEl("timeline");
await until(() => tl.children.length === 2, "two shots on the timeline");
tl.children[0].click();
await until(() => !getEl("inspector").hidden, "inspector after selecting a shot");
getEl("inSpeed").value = "0.5";
for (const f of getEl("inSpeed").__listeners.change || []) f({});
getEl("tlRight").click();                                  // move shot 1 later
await until(() => !getEl("pendingBar").hidden, "pending changes bar");
if (!/2 unsaved changes/.test(getEl("pendingText").textContent)) failures.push("pending text: " + getEl("pendingText").textContent);
getEl("pendingApply").click();
await until(() => edits.length === 1, "the batched edit request");
if (edits[0] && (edits[0].length !== 2 || edits[0][0].op !== "set" || edits[0][1].op !== "move")) {
  failures.push("unexpected ops: " + JSON.stringify(edits[0]));
}

const chunkPuts = requests.filter((r) => r.startsWith("PUT") && r.includes("/chunk"));
if (chunkPuts.length !== 5) failures.push(`expected 5 chunk uploads (3+2), saw ${chunkPuts.length}`);
if (!requests.some((r) => r.endsWith("/commit"))) failures.push("never committed the upload to the library");
if (!requests.some((r) => r.includes("/generate"))) failures.push("never started generation");
if (errors.length) failures.push(...errors);
if (toasts.length) failures.push("the app showed an error: " + toasts.join(" | "));
if (getEl("player").hidden) failures.push("player not shown after generation");

if (failures.length) {
  console.error("UI SMOKE TEST FAILED:\n - " + failures.join("\n - "));
  process.exit(1);
}
console.log("ui smoke ok: sign-in, project, " + chunkPuts.length + " chunks, commit, generate, timeline edit");
process.exit(0);
