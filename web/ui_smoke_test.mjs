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
      if (el.id === "toasts") toasts.push(c.textContent);
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
const hiddenInit = ["viewWork", "viewResult", "refBlock", "banner", "recent", "updatePill"];
for (const id of hiddenInit) getEl(id).hidden = true;

const requests = [];
const errors = [];
let jobPolls = 0;

async function fakeFetch(url, opts = {}) {
  const u = String(url);
  const method = (opts.method || "GET").toUpperCase();
  requests.push(`${method} ${u}`);
  const json = (obj, status = 200) => ({ ok: status < 300, status, json: async () => obj, arrayBuffer: async () => new ArrayBuffer(0) });
  if (u.endsWith("/api/health")) return json({ ok: true, max_clips: 200, max_upload_mb: 100000, ffmpeg: true, version: "dev", extra: {} });
  if (u.endsWith("/api/me")) return json({ prefs: {}, recent: [] });
  if (u.includes("/api/update")) return json({ enabled: false });
  if (u.endsWith("/api/me/prefs")) return json({ ok: true });
  if (u.endsWith("/api/ping")) return json({ ok: true });
  if (u.endsWith("/api/uploads") && method === "POST") return json({ upload_id: "u".repeat(32), chunk_bytes: 4 * 1048576 }, 201);
  if (u.endsWith("/api/jobs") && method === "POST") {
    const body = JSON.parse(opts.body);
    if (body.upload_id !== "u".repeat(32)) return json({ error: "bad upload id" }, 400);
    return json({ id: "j".repeat(32) }, 202);
  }
  if (/\/api\/jobs\/j+$/.test(u)) {
    jobPolls++;
    return json({ id: "j".repeat(32), status: jobPolls < 3 ? "running" : "done", stage: jobPolls < 3 ? "Analyzing clips (1 of 2)" : "Done",
      progress: jobPolls < 3 ? 0.4 : 1, revision: 1, planner: "rule-based", can_undo: false,
      edl: { cuts: [{ clip: "/x/clip000.mp4", start: 0, end: 2 }, { clip: "/x/clip001.mp4", start: 0, end: 3 }] } });
  }
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
const win = {
  SPLICE_API: "",
  addEventListener() {},
  scrollTo() {},
  location: { hostname: "localhost" },
};
const ctx = {
  window: win,
  document: { getElementById: getEl, createElement: () => makeEl("_c"), querySelector: () => makeEl("_q") },
  localStorage: { getItem: (k) => store.get(k) ?? null, setItem: (k, v) => store.set(k, v), removeItem: (k) => store.delete(k) },
  location: win.location, navigator: {}, crypto: globalThis.crypto, performance: globalThis.performance,
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
await sleep(50);

// drop two clips (10 MB and 6 MB => several chunks each, spanning 4 MB chunks)
const big = new File([new Uint8Array(10 * 1048576)], "a.mp4", { type: "video/mp4" });
const small = new File([new Uint8Array(6 * 1048576)], "b.mp4", { type: "video/mp4" });
const drop = getEl("drop");
for (const f of drop.__listeners.drop || []) f({ preventDefault() {}, dataTransfer: { files: [big, small] } });
getEl("prompt").value = "calm";
getEl("go").click();

const deadline = Date.now() + 15000;
while (Date.now() < deadline && getEl("viewResult").hidden && !errors.length) await sleep(50);

const failures = [];
if (errors.length) failures.push(...errors);
if (toasts.length) failures.push("the app showed a message: " + toasts.join(" | "));
if (getEl("viewResult").hidden) failures.push("never reached the result view; stuck at: " + getEl("workTitle").textContent + " / " + getEl("workSub").textContent);
const chunkPuts = requests.filter((r) => r.startsWith("PUT") && r.includes("/chunk"));
if (chunkPuts.length !== 5) failures.push(`expected 5 chunk uploads (3+2), saw ${chunkPuts.length}: ${chunkPuts.join(" | ")}`);
if (!requests.includes("POST /api/jobs")) failures.push("never created the job");
if (getEl("go").disabled && getEl("viewResult").hidden) failures.push("Create button left disabled");

if (failures.length) {
  console.error("UI SMOKE TEST FAILED:\n - " + failures.join("\n - "));
  process.exit(1);
}
console.log("ui smoke ok: " + chunkPuts.length + " chunks uploaded, job created, result shown");
process.exit(0);
