(() => {
  "use strict";

  // ═════════════ basics ═════════════
  const API = (window.SPLICE_API || "").replace(/\/$/, "");
  const $ = (id) => document.getElementById(id);
  const store = {
    get(k) { try { return localStorage.getItem(k); } catch { return null; } },
    set(k, v) { try { localStorage.setItem(k, v); } catch { /* private mode */ } },
    del(k) { try { localStorage.removeItem(k); } catch { /* ignore */ } },
  };
  const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  const fmtMB = (b) => (b / 1048576 >= 10 ? Math.round(b / 1048576) : (b / 1048576).toFixed(1)) + " MB";
  const fmtRate = (bps) => (bps >= 1048576 ? (bps / 1048576).toFixed(1) + " MB/s" : Math.round(bps / 1024) + " KB/s");
  const fmtETA = (s) => (!isFinite(s) ? "…" : s < 60 ? Math.ceil(s) + "s" : Math.ceil(s / 60) + " min");
  const fmtDur = (s) => { s = Math.max(0, Math.round(s)); return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`; };
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

  // Anonymous browser-local id: used only when accounts are off.
  const visitor = (() => {
    let v = store.get("splice.visitor");
    if (!v || !/^[0-9a-f]{32}$/.test(v)) {
      const b = crypto.getRandomValues(new Uint8Array(16));
      v = Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
      store.set("splice.visitor", v);
    }
    return v;
  })();

  const state = {
    user: null,
    authRequired: false,
    authMode: "login",
    provider: store.get("splice.provider") || "claude",
    key: store.get("splice.key") || "",
    connected: false,
    limits: { clips: 200, mb: 100000 },
    localUrls: null,
    p: null,            // current project (ProjectView)
    briefMode: "describe",
    settingsTimer: null,
    uploading: false,
    generating: false,
    pollToken: 0,
    libPick: false,
    pickSel: new Set(),
  };

  function toast(msg, kind = "") {
    const el = document.createElement("div");
    el.className = "toast " + kind;
    el.textContent = msg;
    $("toasts").appendChild(el);
    setTimeout(() => el.remove(), kind === "err" ? 7000 : 3800);
  }

  function aiHeaders() {
    if (!state.connected || !state.key) return {};
    return { "X-AI-Provider": state.provider, "X-AI-Key": state.key };
  }

  class ApiError extends Error {
    constructor(msg, status, body) { super(msg); this.status = status; this.body = body; }
  }

  async function api(path, opts = {}) {
    const headers = { "X-Splice": "1", "X-Visitor": visitor, ...(opts.headers || {}) };
    let body = opts.body;
    if (body && typeof body === "object" && !(body instanceof Blob) && !(body instanceof FormData) && !(body instanceof ArrayBuffer)) {
      headers["Content-Type"] = "application/json";
      body = JSON.stringify(body);
    }
    const res = await fetch(API + path, { ...opts, headers, body });
    let data = null;
    try { data = await res.json(); } catch { /* not JSON */ }
    if (res.status === 401 && state.authRequired && !path.startsWith("/api/auth/")) {
      state.user = null;
      renderChrome();
      go("#/auth");
      throw new ApiError("Please sign in again.", 401, data);
    }
    if (!res.ok) throw new ApiError((data && data.error) || `Request failed (${res.status})`, res.status, data);
    return data;
  }

  // ═════════════ views + routing ═════════════
  const VIEWS = ["viewAuth", "viewHome", "viewLibrary", "viewProject"];
  function show(view) {
    for (const id of VIEWS) $(id).hidden = id !== view;
    window.scrollTo({ top: 0 });
  }
  function go(hash) {
    if (location.hash === hash) route();
    else location.hash = hash;
  }

  async function route() {
    const h = location.hash || "#/home";
    state.pollToken++; // stop any project polling when navigating
    if (state.authRequired && !state.user) { showAuth(); return; }
    if (h.startsWith("#/p/")) { await openProject(h.slice(4)); return; }
    if (h.startsWith("#/library")) { await openLibrary(); return; }
    await openHome();
  }

  function renderChrome() {
    const signedIn = !!state.user || !state.authRequired;
    $("nav").hidden = !signedIn;
    $("acctBtn").hidden = !state.user;
    if (state.user) $("acctName").textContent = state.user.name || state.user.email;
    const h = location.hash || "#/home";
    $("navHome").classList.toggle("on", h.startsWith("#/home") || h.startsWith("#/p/") || h === "");
    $("navLib").classList.toggle("on", h.startsWith("#/library"));
  }

  // ═════════════ auth ═════════════
  function showAuth() {
    renderChrome();
    show("viewAuth");
    $("authErr").hidden = true;
    setAuthMode(state.authMode);
  }

  function setAuthMode(mode) {
    state.authMode = mode;
    for (const b of $("authTabs").children) b.setAttribute("aria-selected", String(b.dataset.v === mode));
    $("nameField").hidden = mode !== "signup";
    $("authGoText").textContent = mode === "signup" ? "Create account" : "Sign in";
    $("authPassword").autocomplete = mode === "signup" ? "new-password" : "current-password";
  }

  async function submitAuth(e) {
    e.preventDefault();
    const err = $("authErr");
    err.hidden = true;
    const email = $("authEmail").value.trim();
    const password = $("authPassword").value;
    if (!email || !password) { err.textContent = "Enter your email and password."; err.hidden = false; return; }
    const btn = $("authGo");
    btn.disabled = true;
    try {
      const path = state.authMode === "signup" ? "/api/auth/signup" : "/api/auth/login";
      const r = await api(path, { method: "POST", body: { email, password, name: $("authName").value.trim() } });
      state.user = r.user;
      $("authPassword").value = "";
      renderChrome();
      toast(state.authMode === "signup" ? "Welcome! Your account is ready." : "Signed in.", "ok");
      if (location.hash === "#/auth" || !location.hash) go("#/home"); else await route();
    } catch (ex) {
      err.textContent = ex.message;
      err.hidden = false;
    } finally {
      btn.disabled = false;
    }
  }

  async function signOut() {
    try { await api("/api/auth/logout", { method: "POST" }); } catch { /* already out */ }
    state.user = null;
    $("acctDialog").close();
    renderChrome();
    showAuth();
  }

  async function changePassword() {
    const err = $("pwErr");
    err.hidden = true;
    try {
      await api("/api/auth/password", { method: "POST", body: { current: $("pwCurrent").value, next: $("pwNext").value } });
      $("pwCurrent").value = ""; $("pwNext").value = "";
      toast("Password updated. Other devices were signed out.", "ok");
    } catch (ex) {
      err.textContent = ex.message; err.hidden = false;
    }
  }

  // ═════════════ confirm dialog ═════════════
  function confirmBox(title, body, yes = "Confirm") {
    $("confirmTitle").textContent = title;
    $("confirmBody").textContent = body;
    $("confirmYes").textContent = yes;
    const dlg = $("confirmDialog");
    return new Promise((resolve) => {
      const done = (v) => { dlg.close(); resolve(v); };
      $("confirmYes").onclick = () => done(true);
      $("confirmNo").onclick = () => done(false);
      dlg.addEventListener("cancel", () => resolve(false), { once: true });
      dlg.showModal();
    });
  }

  // ═════════════ home: projects ═════════════
  const STATUS_LABEL = { draft: "Draft", queued: "Queued", running: "Rendering", done: "Ready", error: "Needs attention" };

  async function openHome() {
    renderChrome();
    show("viewHome");
    try {
      const { projects } = await api("/api/projects");
      renderProjects(projects || []);
    } catch (e) {
      if (e.status !== 401) toast(e.message, "err");
    }
  }

  function when(iso) {
    const d = new Date(iso);
    return d.toLocaleDateString([], { month: "short", day: "numeric" }) + ", " +
      d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
  }

  function renderProjects(list) {
    const grid = $("projectGrid");
    grid.innerHTML = "";
    $("homeEmpty").hidden = list.length > 0;
    for (const p of list) {
      const li = document.createElement("li");
      li.className = "card-item";
      const cover = p.cover ? `<img alt="" src="${API}/api/media/${esc(p.cover)}/thumb" loading="lazy">` : `<span class="cover-ph">🎬</span>`;
      li.innerHTML =
        `<button class="card-open" type="button" aria-label="Open ${esc(p.name)}">` +
        `<span class="cover">${cover}<span class="chip ${esc(p.status)}">${esc(STATUS_LABEL[p.status] || p.status)}</span></span>` +
        `<span class="card-name">${esc(p.name)}</span>` +
        `<span class="card-meta">${esc(p.preset)} · ${p.media} clip${p.media === 1 ? "" : "s"}` +
        `${p.rendered ? " · " + fmtDur(p.length) : ""} · ${esc(when(p.created))}</span></button>` +
        `<button class="x del" type="button" aria-label="Delete ${esc(p.name)}" title="Delete project">×</button>`;
      li.querySelector(".card-open").addEventListener("click", () => go("#/p/" + p.id));
      li.querySelector(".del").addEventListener("click", async () => {
        if (!(await confirmBox("Delete this project?", `“${p.name}” and its edit history will be removed. Your footage stays in the library.`, "Delete"))) return;
        try { await api("/api/projects/" + p.id, { method: "DELETE" }); toast("Project deleted."); openHome(); }
        catch (e) { toast(e.message, "err"); }
      });
      grid.appendChild(li);
    }
  }

  async function newProject() {
    try {
      const p = await api("/api/projects", { method: "POST", body: { name: "Untitled project" } });
      go("#/p/" + p.id);
    } catch (e) { toast(e.message, "err"); }
  }

  // ═════════════ library ═════════════
  async function openLibrary() {
    renderChrome();
    show("viewLibrary");
    await refreshLibrary();
  }

  async function refreshLibrary() {
    try {
      const r = await api("/api/media");
      const items = (r.media || []).filter((m) => m.kind === "clip");
      $("libEmpty").hidden = items.length > 0;
      $("libSub").textContent = items.length
        ? `${items.length} clip${items.length === 1 ? "" : "s"} · ${fmtMB(r.bytes || 0)} on this computer`
        : "Upload once, use in any project.";
      const grid = $("libGrid");
      grid.innerHTML = "";
      for (const m of items) {
        const li = document.createElement("li");
        li.className = "card-item media-item";
        li.innerHTML =
          `<span class="cover"><img alt="" src="${API}/api/media/${esc(m.id)}/thumb" loading="lazy"></span>` +
          `<span class="card-name">${esc(m.name)}</span>` +
          `<span class="card-meta">${fmtDur(m.duration)} · ${m.width}×${m.height} · ${fmtMB(m.size)}</span>` +
          `<button class="x del" type="button" aria-label="Delete ${esc(m.name)}" title="Remove from library">×</button>`;
        li.querySelector(".del").addEventListener("click", () => deleteMedia(m));
        grid.appendChild(li);
      }
      return items;
    } catch (e) { if (e.status !== 401) toast(e.message, "err"); return []; }
  }

  async function deleteMedia(m) {
    if (!(await confirmBox("Remove this footage?", `“${m.name}” will be deleted from this computer's library.`, "Remove"))) return;
    try {
      await api("/api/media/" + m.id, { method: "DELETE" });
      refreshLibrary();
    } catch (e) {
      if (e.status === 409) {
        if (await confirmBox("It's in use", `${e.message}. Remove it anyway? Those projects will lose this clip.`, "Remove anyway")) {
          await api("/api/media/" + m.id + "?force=1", { method: "DELETE" });
          refreshLibrary();
        }
      } else toast(e.message, "err");
    }
  }

  // ═════════════ uploads (chunked, parallel, resumable) ═════════════
  const PARALLEL = 4;

  function putChunk(url, headers, blob, onProgress) {
    return new Promise((resolve, reject) => {
      const xhr = new XMLHttpRequest();
      xhr.open("PUT", url);
      for (const [k, v] of Object.entries(headers)) xhr.setRequestHeader(k, v);
      xhr.upload.onprogress = (e) => { if (e.lengthComputable) onProgress(e.loaded); };
      xhr.onload = () => {
        let body = {};
        try { body = JSON.parse(xhr.responseText); } catch { /* ignore */ }
        if (xhr.status >= 200 && xhr.status < 300) return resolve(body);
        const err = new Error(body.error || `Upload failed (${xhr.status})`);
        err.status = xhr.status;
        err.fatal = xhr.status >= 400 && xhr.status < 500 && xhr.status !== 408 && xhr.status !== 429;
        reject(err);
      };
      xhr.onerror = () => reject(new Error("Connection dropped"));
      xhr.send(blob);
    });
  }

  // items: [{ file, kind: "clip"|"inspo"|"music", index }] -> upload id
  async function uploadAll(items, ui) {
    const sess = await api("/api/uploads", { method: "POST" });
    const chunk = sess.chunk_bytes || 32 * 1048576;
    const tasks = [];
    for (const it of items) for (let off = 0; off < it.file.size; off += chunk) tasks.push({ it, off });
    const total = items.reduce((a, it) => a + it.file.size, 0);
    const inflight = new Map();
    let done = 0, retrying = 0;
    const t0 = Date.now();

    const render = () => {
      let sent = done;
      for (const v of inflight.values()) sent += v;
      const secs = Math.max(1, (Date.now() - t0) / 1000);
      const rate = sent / secs;
      ui.progress(sent / total, `${fmtMB(sent)} of ${fmtMB(total)}`);
      let line;
      if (retrying > 0) line = "Connection hiccup, retrying…";
      else if (sent === 0) line = secs < 8 ? "Connecting…" : "Still waiting for the connection to start moving data…";
      else {
        const eta = (total - sent) / Math.max(1, rate);
        line = `${fmtRate(rate)} · about ${fmtETA(eta)} left`;
        if (secs > 6 && eta > 20 * 60) line += " — a long upload for this connection; fewer or shorter clips will be much faster.";
      }
      ui.speed(line);
    };
    const tick = setInterval(render, 500);
    render();

    let next = 0;
    const worker = async () => {
      while (next < tasks.length) {
        const task = tasks[next++];
        const { it, off } = task;
        const blob = it.file.slice(off, Math.min(it.file.size, off + chunk));
        const headers = {
          "X-Splice": "1", "X-Visitor": visitor,
          "X-Kind": it.kind, "X-Index": String(it.index), "X-Name": it.file.name,
          "X-Offset": String(off), "X-Total": String(it.file.size),
        };
        let attempt = 0;
        for (;;) {
          try {
            inflight.set(task, 0);
            await putChunk(`${API}/api/uploads/${sess.upload_id}/chunk`, headers, blob, (n) => inflight.set(task, n));
            inflight.delete(task);
            done += blob.size;
            break;
          } catch (e) {
            inflight.delete(task);
            if (e.status === 404) throw new Error("The editor restarted while you were uploading. Please add the files again.");
            if (e.fatal || ++attempt > 5) throw e;
            retrying++;
            await sleep(600 * 2 ** attempt);
            retrying--;
          }
        }
      }
    };
    try {
      await Promise.all(Array.from({ length: Math.min(PARALLEL, tasks.length) }, worker));
    } finally {
      clearInterval(tick);
    }
    return sess.upload_id;
  }

  // Time a real 1 MB upload so a slow path is reported before a long wait.
  async function probeSpeed() {
    const blob = new Blob([new Uint8Array(1 << 20)]);
    const t = performance.now();
    const res = await fetch(API + "/api/ping", { method: "POST", body: blob, headers: { "X-Splice": "1", "X-Visitor": visitor } });
    if (!res.ok) throw new Error("The editor didn't answer the connection check.");
    await res.arrayBuffer();
    return blob.size / Math.max(0.05, (performance.now() - t) / 1000);
  }

  function askAboutSlowUpload(bytes, rate) {
    const dlg = $("slowDialog");
    const mins = bytes / rate / 60;
    const eta = mins >= 90 ? (mins / 60).toFixed(1) + " hours" : Math.round(mins) + " minutes";
    $("slowBody").textContent = `Your connection to the editor runs at about ${fmtRate(rate)}. Uploading ${fmtMB(bytes)} would take roughly ${eta}.`;
    const tips = [
      "<b>You rarely need that much.</b> A 30-second video usually only needs a few minutes of footage: try fewer or shorter clips.",
      "<b>Trim first.</b> Cut clips down in your phone's photo app before uploading.",
    ];
    if (state.localUrls && state.localUrls.length) {
      tips.unshift("<b>You're on the same network as the editor.</b> Use the direct address and this takes seconds: " +
        state.localUrls.map((u) => `<a href="${esc(u)}" style="color:#fff">${esc(u.replace("http://", ""))}</a>`).join(" or "));
    }
    $("slowTips").innerHTML = tips.map((t) => `<li>${t}</li>`).join("");
    return new Promise((resolve) => {
      const done = (v) => { dlg.close(); resolve(v); };
      $("slowGo").onclick = () => done(true);
      $("slowCancel").onclick = () => done(false);
      dlg.addEventListener("cancel", () => resolve(false), { once: true });
      dlg.showModal();
    });
  }

  // Upload files into the library; returns the committed media list (or null if cancelled).
  async function uploadToLibrary(files, kind) {
    const items = files.map((file, index) => ({ file, kind, index: kind === "clip" ? index : 0 }));
    const total = items.reduce((a, it) => a + it.file.size, 0);
    if (total > 100 * 1048576) {
      const rate = await probeSpeed();
      if (total / rate > 10 * 60 && !(await askAboutSlowUpload(total, rate))) return null;
    }
    state.uploading = true;
    const card = $("uploadCard");
    card.hidden = false;
    $("upTitle").textContent = `Uploading ${files.length} file${files.length === 1 ? "" : "s"}`;
    $("upBar").style.width = "0%";
    try {
      const uploadId = await uploadAll(items, {
        progress: (f, text) => { $("upBar").style.width = Math.round(f * 100) + "%"; $("upSub").textContent = text; },
        speed: (line) => { $("upSpeed").textContent = line; },
      });
      $("upTitle").textContent = "Adding to your library";
      $("upSpeed").textContent = "Reading the footage…";
      const r = await api(`/api/uploads/${uploadId}/commit`, { method: "POST" });
      if (r.problems && r.problems.length) toast(r.problems.join(" · "), "err");
      return r.media || [];
    } catch (e) {
      if (e.body && e.body.problems && e.body.problems.length) throw new Error(e.body.problems.join(" · "));
      throw e;
    } finally {
      state.uploading = false;
      card.hidden = true;
    }
  }

  // ═════════════ project workspace ═════════════
  async function openProject(id) {
    renderChrome();
    show("viewProject");
    let p;
    try { p = await api("/api/projects/" + id); }
    catch (e) { toast(e.status === 404 ? "That project doesn't exist." : e.message, "err"); go("#/home"); return; }
    state.p = p;
    fillProject();
    if (p.status === "queued" || p.status === "running") startPolling();
  }

  function fillProject() {
    const p = state.p;
    $("projName").value = p.name || "";
    const s = p.settings || {};
    $("prompt").value = s.prompt || "";
    $("title").value = s.title || "";
    const dur = Math.min(90, Math.max(10, Math.round(s.duration || 30)));
    $("duration").value = String(dur);
    $("durOut").textContent = dur + "s";
    for (const b of $("presets").children) b.setAttribute("aria-checked", String(b.dataset.v === (s.preset || "9:16")));
    setBriefMode(s.inspo_url || s.inspo_media ? "reference" : "describe");
    $("refUrl").value = s.inspo_url || "";
    $("refText").textContent = s.inspo_media ? "Reference video attached" : "Upload a reference video…";
    $("refLabel").classList.toggle("has", !!s.inspo_media);
    $("refClear").hidden = !s.inspo_media;
    $("musicText").textContent = s.music_media ? "Music attached" : "Choose a track…";
    $("musicLabel").classList.toggle("has", !!s.music_media);
    $("musicClear").hidden = !s.music_media;
    renderClips();
    renderStatus();
    updateGo();
  }

  function setBriefMode(mode) {
    state.briefMode = mode;
    for (const x of $("briefMode").children) x.setAttribute("aria-checked", String(x.dataset.v === mode));
    const ref = mode === "reference";
    $("refBlock").hidden = !ref;
    $("ideas").hidden = ref;
    $("promptLabel").textContent = ref ? "Anything to add? (optional)" : "What should this video feel like?";
    $("prompt").placeholder = ref ? "e.g. but keep it a bit calmer" : "e.g. Fast-paced travel reel, open on the best view, keep it upbeat";
  }

  function currentSettings() {
    const preset = [...$("presets").children].find((b) => b.getAttribute("aria-checked") === "true");
    const prev = (state.p && state.p.settings) || {};
    const ref = state.briefMode === "reference";
    return {
      prompt: $("prompt").value.trim(),
      preset: preset ? preset.dataset.v : "9:16",
      duration: Number($("duration").value) || 30,
      title: $("title").value.trim(),
      inspo_url: ref && !prev.inspo_media ? $("refUrl").value.trim() : "",
      inspo_media: ref ? (prev.inspo_media || "") : "",
      music_media: prev.music_media || "",
    };
  }

  async function saveSettingsNow() {
    clearTimeout(state.settingsTimer);
    if (!state.p) return;
    try {
      const p = await api("/api/projects/" + state.p.id, { method: "PATCH", body: { settings: currentSettings() } });
      state.p = { ...state.p, settings: p.settings };
    } catch (e) { if (e.status !== 409) toast(e.message, "err"); }
  }
  function saveSettingsSoon() {
    clearTimeout(state.settingsTimer);
    state.settingsTimer = setTimeout(saveSettingsNow, 600);
  }

  function renderClips() {
    const ul = $("clipList");
    ul.innerHTML = "";
    const media = state.p.media || [];
    media.forEach((m) => {
      const li = document.createElement("li");
      li.className = "clip";
      li.innerHTML =
        `<img alt="" src="${API}/api/media/${esc(m.id)}/thumb">` +
        `<div><div class="nm">${esc(m.name)}</div><div class="sz">${fmtDur(m.duration)} · ${fmtMB(m.size)}</div></div>` +
        `<button class="x" type="button" aria-label="Remove ${esc(m.name)}">×</button>`;
      li.querySelector("button").addEventListener("click", () => removeClip(m.id));
      ul.appendChild(li);
    });
    const secs = media.reduce((a, m) => a + (m.duration || 0), 0);
    $("clipMeta").textContent = media.length ? `${media.length} clip${media.length > 1 ? "s" : ""} · ${fmtDur(secs)}` : "";
  }

  async function setMedia(ids) {
    const p = await api("/api/projects/" + state.p.id, { method: "PATCH", body: { media: ids } });
    state.p = { ...state.p, media: p.media };
    renderClips();
    updateGo();
  }

  async function removeClip(id) {
    try { await setMedia(state.p.media.filter((m) => m.id !== id).map((m) => m.id)); }
    catch (e) { toast(e.message, "err"); }
  }

  async function addFilesToProject(fileList) {
    const all = [...fileList];
    const files = all.filter((f) => f.type.startsWith("video/") || /\.(mp4|mov|m4v|mkv|webm)$/i.test(f.name));
    if (files.length !== all.length) toast("Only video files can be added as footage.", "err");
    if (!files.length || state.uploading || !state.p) return;
    try {
      const added = await uploadToLibrary(files, "clip");
      if (!added) return;
      const have = state.p.media.map((m) => m.id);
      const ids = [...have, ...added.map((m) => m.id).filter((id) => !have.includes(id))];
      await setMedia(ids);
      const dups = added.filter((m) => m.existing).length;
      if (dups) toast(`${dups} clip${dups > 1 ? "s were" : " was"} already in your library, so nothing was re-uploaded.`);
    } catch (e) { toast(e.message, "err"); }
  }

  async function attachSingle(file, kind, field) {
    if (!file || state.uploading || !state.p) return;
    try {
      const added = await uploadToLibrary([file], kind);
      if (!added || !added[0]) return;
      await api("/api/projects/" + state.p.id, { method: "PATCH", body: { settings: { ...currentSettings(), [field]: added[0].id } } });
      state.p = await api("/api/projects/" + state.p.id);
      fillProject();
    } catch (e) { toast(e.message, "err"); }
  }

  async function openPicker() {
    let items = [];
    try { items = ((await api("/api/media")).media || []).filter((m) => m.kind === "clip"); } catch { /* empty list */ }
    const have = new Set((state.p.media || []).map((m) => m.id));
    state.pickSel = new Set();
    const ul = $("pickList");
    ul.innerHTML = "";
    const avail = items.filter((m) => !have.has(m.id));
    $("pickEmpty").hidden = avail.length > 0;
    $("pickDone").hidden = avail.length === 0;
    for (const m of avail) {
      const li = document.createElement("li");
      li.innerHTML =
        `<label class="pick"><input type="checkbox"><img alt="" src="${API}/api/media/${esc(m.id)}/thumb">` +
        `<span><b>${esc(m.name)}</b><small>${fmtDur(m.duration)} · ${fmtMB(m.size)}</small></span></label>`;
      li.querySelector("input").addEventListener("change", (e) => {
        if (e.target.checked) state.pickSel.add(m.id); else state.pickSel.delete(m.id);
      });
      ul.appendChild(li);
    }
    $("pickDialog").showModal();
  }

  function renderStatus() {
    const p = state.p;
    $("projStatus").textContent = STATUS_LABEL[p.status] || p.status;
    $("projStatus").className = "chip " + p.status;
    const done = p.status === "done" && p.revision > 0;
    $("playerEmpty").hidden = done;
    $("player").hidden = !done;
    $("reviseCard").hidden = !done;
    $("timelineCard").hidden = !done;
    $("resultActions").hidden = !done;
    $("goText").textContent = done ? "Regenerate" : "Create my edit";
    if (done) {
      const url = `${API}/api/projects/${p.id}/video?r=${p.revision}`;
      if ($("player").getAttribute("data-src") !== url) {
        $("player").setAttribute("data-src", url);
        $("player").src = url;
        $("player").load();
      }
      $("download").href = url;
      $("download").setAttribute("download", (p.name || "splice-edit").replace(/[^\w\- ]+/g, "") + ".mp4");
      $("undoBtn").disabled = !p.can_undo;
      renderTimeline(p.edl);
      const fixes = p.edl && p.edl.fixes ? Object.keys(p.edl.fixes).length : 0;
      $("resultMeta").innerHTML =
        `<span class="pill">${p.planner === "rule-based" ? "Automatic edit" : "Edited by " + esc(p.planner)}</span>` +
        (fixes ? `<span class="pill">${fixes} clip${fixes > 1 ? "s" : ""} rotated upright</span>` : "") +
        `<span class="pill">${esc((p.settings || {}).preset || "")}</span><span class="pill">v${p.revision}</span>`;
    } else {
      $("resultMeta").textContent = "";
    }
    const notes = $("resultNotes");
    notes.innerHTML = "";
    for (const n of (p.notes || [])) {
      const li = document.createElement("li");
      li.textContent = n;
      notes.appendChild(li);
    }
    if (p.status === "error" && p.error) {
      const li = document.createElement("li");
      li.textContent = p.error;
      notes.appendChild(li);
    }
    $("reviseHint").textContent = state.connected
      ? "AI is connected, so you can ask for anything."
      : "Works without AI for common changes (speed, mute, remove, titles, zoom…). Connect AI for open-ended requests.";
  }

  const PALETTE = ["#7c5cff", "#ff5c93", "#ffb35c", "#3ddc97", "#4cc3ff", "#c46bff", "#ff7a59", "#8bd450"];
  function renderTimeline(edl) {
    const cuts = (edl && edl.cuts) || [];
    const durs = cuts.map((c) => Math.max(0.1, (c.end - c.start) / (c.speed > 0 ? c.speed : 1)));
    const total = durs.reduce((a, b) => a + b, 0) || 1;
    const names = [];
    const byFile = new Map();
    for (const m of (state.p.media || [])) byFile.set(m.id, m.name);
    const tl = $("timeline");
    tl.innerHTML = "";
    cuts.forEach((c, i) => {
      const file = c.clip.split("/").pop().replace(/\.[^.]+$/, "");
      const label = byFile.get(file) || file;
      let ci = names.indexOf(label);
      if (ci < 0) { names.push(label); ci = names.length - 1; }
      const el = document.createElement("div");
      el.className = "seg-cut" + (c.caption ? " has-cap" : "") + ((c.speed && c.speed !== 1) || c.zoom > 1 || c.transition ? " has-fx" : "");
      el.style.flex = String(durs[i] / total);
      el.style.background = PALETTE[ci % PALETTE.length];
      el.title = `${i + 1}. ${label}  ${durs[i].toFixed(1)}s` + (c.speed && c.speed !== 1 ? `  ${c.speed}×` : "") + (c.caption ? `\n“${c.caption}”` : "");
      tl.appendChild(el);
    });
    const lg = $("legend");
    lg.innerHTML = "";
    names.forEach((n, i) => {
      const li = document.createElement("li");
      li.innerHTML = `<i style="background:${PALETTE[i % PALETTE.length]}"></i>${esc(n)}`;
      lg.appendChild(li);
    });
    $("tlMeta").textContent = `${cuts.length} cuts · ${fmtDur(total)}`;
  }

  function updateGo() {
    const p = state.p;
    if (!p) return;
    const n = (p.media || []).length;
    const busy = state.generating || state.uploading || p.status === "queued" || p.status === "running";
    $("go").disabled = busy || n === 0;
    $("goHint").textContent = busy ? ""
      : n === 0 ? "Add at least one clip to begin."
        : state.connected ? "Your AI will look at the footage and follow your brief."
          : "No AI connected: you'll get a clean automatic edit. Connect one for smarter cuts.";
  }

  async function generate() {
    if (!state.p || state.generating) return;
    state.generating = true;
    updateGo();
    try {
      await saveSettingsNow();
      await api(`/api/projects/${state.p.id}/generate`, { method: "POST", headers: aiHeaders() });
      state.p.status = "queued";
      renderStatus();
      await startPolling();
    } catch (e) {
      toast(e.message, "err");
    } finally {
      state.generating = false;
      updateGo();
    }
  }

  async function startPolling() {
    const token = ++state.pollToken;
    const id = state.p.id;
    $("progressBlock").hidden = false;
    $("playerBusy").hidden = false;
    let misses = 0;
    for (;;) {
      if (token !== state.pollToken) return; // navigated away
      try {
        const p = await api(`/api/projects/${id}`);
        misses = 0;
        if (token !== state.pollToken) return;
        state.p = p;
        $("workTitle").textContent = p.stage || "Working";
        $("workSub").textContent = p.status === "queued" ? "Waiting in line…" : "";
        $("genBar").style.width = Math.round(Math.max(0.03, p.progress || 0) * 100) + "%";
        $("busyText").textContent = (p.stage || "Working") + "…";
        if (p.status === "done" || p.status === "error" || p.status === "draft") break;
      } catch (e) {
        if (e.status === 401 || e.status === 404 || ++misses > 6) { toast(e.message, "err"); break; }
      }
      await sleep(700);
    }
    $("progressBlock").hidden = true;
    $("playerBusy").hidden = true;
    renderStatus();
    updateGo();
    if (state.p.status === "error") toast(state.p.error || "The edit failed.", "err");
  }

  async function waitRevision(want) {
    const id = state.p.id;
    for (let i = 0; i < 1200; i++) {
      const p = await api(`/api/projects/${id}`);
      if (p.status === "done" && p.error) throw new Error(p.error);
      if (p.status === "done" && p.revision >= want) { state.p = p; renderStatus(); updateGo(); return; }
      if (p.status === "error") throw new Error(p.error || "That change failed.");
      await sleep(500);
    }
    throw new Error("Timed out waiting for the edit.");
  }

  async function revise(instruction) {
    if (!state.p) return;
    const input = $("reviseInput"), btn = $("reviseGo");
    input.disabled = btn.disabled = true;
    $("playerBusy").hidden = false;
    $("busyText").textContent = "Applying your change…";
    const want = state.p.revision + 1;
    try {
      await api(`/api/projects/${state.p.id}/revise`, { method: "POST", headers: aiHeaders(), body: { instruction } });
      await waitRevision(want);
      input.value = "";
      toast("Done: your change is in.", "ok");
    } catch (e) { toast(e.message, "err"); }
    finally { input.disabled = btn.disabled = false; $("playerBusy").hidden = true; }
  }

  async function undo() {
    if (!state.p) return;
    $("undoBtn").disabled = true;
    $("playerBusy").hidden = false;
    $("busyText").textContent = "Undoing…";
    const want = state.p.revision + 1;
    try {
      await api(`/api/projects/${state.p.id}/undo`, { method: "POST" });
      await waitRevision(want);
      toast("Undone.", "ok");
    } catch (e) { toast(e.message, "err"); }
    finally { $("playerBusy").hidden = true; }
  }

  // ═════════════ AI connection ═════════════
  const KEY_HELP = {
    claude: 'Get a key at <a href="https://console.anthropic.com/settings/keys" target="_blank" rel="noopener">console.anthropic.com</a>. Usage is billed to your Anthropic account (a typical edit costs a few cents).',
    gemini: 'Get a free key at <a href="https://aistudio.google.com/apikey" target="_blank" rel="noopener">aistudio.google.com</a>. The free tier has daily limits.',
  };

  function renderAI() {
    $("aiPill").classList.toggle("on", state.connected);
    $("aiPillText").textContent = state.connected ? (state.provider === "claude" ? "Claude connected" : "Gemini connected") : "AI: optional";
    $("aiForget").hidden = !state.connected;
    for (const b of $("provider").children) b.setAttribute("aria-checked", String(b.dataset.v === state.provider));
    $("keyHelp").innerHTML = KEY_HELP[state.provider];
    $("aiKey").placeholder = state.provider === "claude" ? "sk-ant-…" : "AIza…";
    if (state.p) updateGo();
  }

  async function connect() {
    const key = $("aiKey").value.trim();
    const err = $("aiErr");
    err.hidden = true;
    if (!key) { err.textContent = "Paste your API key first."; err.hidden = false; return; }
    const btn = $("aiConnect");
    btn.disabled = true; btn.textContent = "Checking…";
    try {
      const r = await api("/api/verify", { method: "POST", headers: { "X-AI-Provider": state.provider, "X-AI-Key": key } });
      state.key = key; state.connected = true;
      if (r && r.warning) toast(r.warning);
      store.set("splice.provider", state.provider); store.set("splice.key", key);
      $("aiKey").value = "";
      $("aiDialog").close();
      toast("AI connected", "ok");
    } catch (e) { err.textContent = e.message; err.hidden = false; }
    finally { btn.disabled = false; btn.textContent = "Connect"; renderAI(); }
  }

  function disconnect() {
    state.key = ""; state.connected = false;
    store.del("splice.key");
    renderAI();
    $("aiDialog").close();
    toast("Disconnected. Your key was removed from this browser.");
  }

  async function restoreConnection() {
    if (!state.key) return;
    try {
      await api("/api/verify", { method: "POST", headers: { "X-AI-Provider": state.provider, "X-AI-Key": state.key } });
      state.connected = true;
    } catch { state.connected = false; }
    renderAI();
  }

  // ═════════════ app updates (local app only) ═════════════
  async function checkForUpdate() {
    try {
      const u = await api("/api/update");
      if (u.version && u.version !== "dev") $("footVersion").textContent = " · " + u.version;
      if (!u.enabled || !u.available) { $("updatePill").hidden = true; return; }
      $("updateText").textContent = `Update ${u.latest} ready`;
      $("updatePill").title = u.notes || "";
      $("updatePill").hidden = false;
    } catch { /* not the local app, or offline */ }
  }

  async function applyUpdate() {
    const pill = $("updatePill");
    pill.disabled = true;
    $("updateText").textContent = "Updating…";
    try {
      const r = await api("/api/update/apply", { method: "POST" });
      $("updateText").textContent = "Restarting…";
      const t0 = Date.now();
      for (;;) {
        await sleep(700);
        try {
          const h = await api("/api/health");
          if (!r.version || h.version === r.version) break;
        } catch { /* still restarting */ }
        if (Date.now() - t0 > 60000) break;
      }
      location.reload();
    } catch (e) {
      pill.disabled = false;
      $("updateText").textContent = "Update available";
      toast(e.message, "err");
    }
  }

  function showSameNetworkBanner(urls) {
    const b = $("banner");
    b.hidden = false;
    b.style.borderColor = "rgba(61, 220, 151, 0.4)";
    b.style.background = "rgba(61, 220, 151, 0.08)";
    b.style.color = "#c9f5df";
    const links = urls.map((u, i) =>
      `<a href="${esc(u)}" style="color:#fff;font-weight:700">${i === 0 ? "on this computer" : "from another device on your Wi-Fi"}: ${esc(u.replace("http://", ""))}</a>`).join(" &nbsp;·&nbsp; ");
    b.innerHTML = `<b>Faster way:</b> you're on the same network as the editor, so uploading through this link goes out to the internet and back. Use the direct address instead (uploads are instant): ${links}`;
  }

  // ═════════════ wiring ═════════════
  async function addToLibraryOnly(files) {
    const vids = files.filter((f) => f.type.startsWith("video/") || /\.(mp4|mov|m4v|mkv|webm)$/i.test(f.name));
    if (!vids.length || state.uploading) return;
    try {
      const added = await uploadToLibrary(vids, "clip");
      if (added) toast(`${added.length} clip${added.length === 1 ? "" : "s"} added to your library.`, "ok");
      await refreshLibrary();
    } catch (e) { toast(e.message, "err"); }
  }

  function bind() {
    // auth
    $("authTabs").addEventListener("click", (e) => { const b = e.target.closest("button"); if (b) setAuthMode(b.dataset.v); });
    $("authForm").addEventListener("submit", submitAuth);
    $("acctBtn").addEventListener("click", () => { $("acctEmail").textContent = state.user ? state.user.email : ""; $("acctDialog").showModal(); });
    $("signOut").addEventListener("click", signOut);
    $("pwGo").addEventListener("click", changePassword);

    // home + library
    $("newProject").addEventListener("click", newProject);
    $("newProject2").addEventListener("click", newProject);
    $("libAdd").addEventListener("click", () => { state.libPick = true; $("fileInput").click(); });

    // project header
    $("backHome").addEventListener("click", () => go("#/home"));
    const rename = async () => {
      if (!state.p) return;
      const name = $("projName").value.trim();
      if (!name || name === state.p.name) { $("projName").value = state.p.name; return; }
      try { const p = await api("/api/projects/" + state.p.id, { method: "PATCH", body: { name } }); state.p.name = p.name; }
      catch (e) { toast(e.message, "err"); }
    };
    $("projName").addEventListener("blur", rename);
    $("projName").addEventListener("keydown", (e) => { if (e.key === "Enter") { e.preventDefault(); $("projName").blur(); } });

    // footage drop zone
    const drop = $("drop"), input = $("fileInput");
    drop.addEventListener("click", () => { state.libPick = false; input.click(); });
    drop.addEventListener("keydown", (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); state.libPick = false; input.click(); } });
    input.addEventListener("change", async () => {
      const files = [...input.files];
      input.value = "";
      if (!files.length) return;
      if (state.libPick) await addToLibraryOnly(files); else await addFilesToProject(files);
    });
    for (const ev of ["dragenter", "dragover"]) drop.addEventListener(ev, (e) => { e.preventDefault(); drop.classList.add("over"); });
    for (const ev of ["dragleave", "drop"]) drop.addEventListener(ev, (e) => { e.preventDefault(); drop.classList.remove("over"); });
    drop.addEventListener("drop", (e) => addFilesToProject(e.dataTransfer.files));
    window.addEventListener("dragover", (e) => e.preventDefault());
    window.addEventListener("drop", (e) => {
      e.preventDefault();
      if (!$("viewProject").hidden && e.dataTransfer.files.length && !(e.target.closest && e.target.closest("#drop"))) addFilesToProject(e.dataTransfer.files);
    });
    $("pickLibrary").addEventListener("click", openPicker);
    $("pickDone").addEventListener("click", async () => {
      $("pickDialog").close();
      if (!state.pickSel.size) return;
      try { await setMedia([...state.p.media.map((m) => m.id), ...state.pickSel]); }
      catch (e) { toast(e.message, "err"); }
    });

    // vibe + format + music
    $("briefMode").addEventListener("click", (e) => { const b = e.target.closest("button"); if (b) { setBriefMode(b.dataset.v); saveSettingsSoon(); } });
    $("ideas").addEventListener("click", (e) => {
      if (e.target.tagName === "BUTTON") { $("prompt").value = e.target.textContent; $("prompt").focus(); saveSettingsSoon(); }
    });
    for (const id of ["prompt", "title", "refUrl"]) $(id).addEventListener("input", saveSettingsSoon);
    $("duration").addEventListener("input", (e) => { $("durOut").textContent = e.target.value + "s"; saveSettingsSoon(); });
    $("presets").addEventListener("click", (e) => {
      const b = e.target.closest("button"); if (!b) return;
      for (const x of $("presets").children) x.setAttribute("aria-checked", String(x === b));
      saveSettingsSoon();
    });
    $("musicInput").addEventListener("change", (e) => { const f = e.target.files[0]; e.target.value = ""; attachSingle(f, "music", "music_media"); });
    $("musicClear").addEventListener("click", async (e) => {
      e.preventDefault(); e.stopPropagation();
      try {
        await api("/api/projects/" + state.p.id, { method: "PATCH", body: { settings: { ...currentSettings(), music_media: "" } } });
        state.p = await api("/api/projects/" + state.p.id);
        fillProject();
      } catch (x) { toast(x.message, "err"); }
    });
    $("refFile").addEventListener("change", (e) => { const f = e.target.files[0]; e.target.value = ""; attachSingle(f, "inspo", "inspo_media"); });
    $("refClear").addEventListener("click", async (e) => {
      e.preventDefault(); e.stopPropagation();
      try {
        await api("/api/projects/" + state.p.id, { method: "PATCH", body: { settings: { ...currentSettings(), inspo_media: "" } } });
        state.p = await api("/api/projects/" + state.p.id);
        fillProject();
      } catch (x) { toast(x.message, "err"); }
    });

    // generate / revise / undo
    $("go").addEventListener("click", generate);
    $("undoBtn").addEventListener("click", undo);
    $("reviseForm").addEventListener("submit", (e) => { e.preventDefault(); const t = $("reviseInput").value.trim(); if (t) revise(t); });
    $("reviseIdeas").addEventListener("click", (e) => {
      if (e.target.tagName === "BUTTON") { $("reviseInput").value = e.target.textContent; $("reviseInput").focus(); }
    });

    // AI dialog
    $("aiPill").addEventListener("click", () => { $("aiErr").hidden = true; $("aiDialog").showModal(); });
    $("provider").addEventListener("click", (e) => {
      const b = e.target.closest("button"); if (!b) return;
      state.provider = b.dataset.v; $("aiErr").hidden = true; renderAI();
    });
    $("aiConnect").addEventListener("click", connect);
    $("aiKey").addEventListener("keydown", (e) => { if (e.key === "Enter") { e.preventDefault(); connect(); } });
    $("aiForget").addEventListener("click", disconnect);
    $("aiSkip").addEventListener("click", () => $("aiDialog").close());

    $("updatePill").addEventListener("click", applyUpdate);
    window.addEventListener("hashchange", route);
  }

  // ═════════ installable web app + quit ═════════
  function setupInstallAndQuit() {
    let deferred = null;
    window.addEventListener("beforeinstallprompt", (e) => { e.preventDefault(); deferred = e; $("installPill").hidden = false; });
    $("installPill").addEventListener("click", async () => {
      if (!deferred) return;
      deferred.prompt();
      deferred = null;
      $("installPill").hidden = true;
    });
    $("quitBtn").addEventListener("click", async () => {
      if (!(await confirmBox("Quit Splice?", "The app will stop. Your projects and footage are saved; open Splice again any time.", "Quit"))) return;
      try { await api("/api/quit", { method: "POST" }); } catch { /* it may drop the connection as it exits */ }
      document.body.innerHTML = '<main style="max-width:520px;margin:20vh auto;text-align:center;font:16px system-ui;color:#ddd">' +
        '<h1 style="font-size:28px">Splice has stopped</h1><p>You can close this tab. Start Splice again whenever you like.</p></main>';
    });
    if ("serviceWorker" in navigator && /^https?:$/.test(location.protocol)) {
      navigator.serviceWorker.register("sw.js").catch(() => { /* optional */ });
    }
  }

  async function init() {
    bind();
    setupInstallAndQuit();
    renderAI();
    try {
      const h = await api("/api/health");
      if (h.max_clips) state.limits.clips = h.max_clips;
      if (h.max_upload_mb) {
        state.limits.mb = h.max_upload_mb;
        $("limitText").textContent = `MP4, MOV, WebM · up to ${h.max_upload_mb.toLocaleString()} MB per project`;
      }
      if (h.server_ai && !state.key) { state.connected = true; state.provider = "claude"; }
      if (h.local) $("quitBtn").hidden = false;
      if (h.ffmpeg === false) {
        const b = $("banner");
        b.hidden = false;
        b.innerHTML = "<b>One thing missing:</b> the editor can't find <code>ffmpeg</code> on this computer, so videos can't be processed. " +
          "Install it (Mac: <code>brew install ffmpeg</code>, Windows: <code>winget install Gyan.FFmpeg</code>) and restart the app.";
      }
      const ex = h.extra || {};
      if (ex.same_network && Array.isArray(ex.local_urls) && !/^(localhost|127\.0\.0\.1)$/.test(location.hostname)) {
        state.localUrls = ex.local_urls;
        showSameNetworkBanner(ex.local_urls);
      }
    } catch {
      const b = $("banner");
      b.innerHTML = "<b>Can't reach the editor.</b> If this is the downloaded app, make sure it's running.";
      b.hidden = false;
    }
    try {
      const me = await api("/api/auth/me");
      state.authRequired = !!me.required;
      state.user = me.user || null;
    } catch { /* treat as no accounts */ }
    renderAI();
    renderChrome();
    await route();
    restoreConnection();
    checkForUpdate();
    setInterval(checkForUpdate, 15 * 60 * 1000);
  }

  window.addEventListener("unhandledrejection", (e) => {
    console.error(e.reason);
    toast((e.reason && e.reason.message) || "Something went wrong.", "err");
  });

  init();
})();
