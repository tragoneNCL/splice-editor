(() => {
  "use strict";

  const API = (window.SPLICE_API || "").replace(/\/$/, "");
  const $ = (id) => document.getElementById(id);
  const store = {
    get(k) { try { return localStorage.getItem(k); } catch { return null; } },
    set(k, v) { try { localStorage.setItem(k, v); } catch { /* private mode */ } },
    del(k) { try { localStorage.removeItem(k); } catch { /* ignore */ } },
  };

  const state = {
    clips: [],            // File[]
    music: null,          // File | null
    preset: "9:16",
    provider: store.get("splice.provider") || "claude",
    key: store.get("splice.key") || "",
    connected: false,
    limits: { clips: 12, mb: 400 },
    job: null,            // { id, revision }
    busy: false,
  };

  // ───────── helpers ─────────
  const fmtMB = (b) => (b / 1048576 >= 10 ? Math.round(b / 1048576) : (b / 1048576).toFixed(1)) + " MB";
  const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

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

  async function api(path, opts = {}) {
    const res = await fetch(API + path, opts);
    let body = null;
    try { body = await res.json(); } catch { /* not json */ }
    if (!res.ok) throw new Error((body && body.error) || `Request failed (${res.status})`);
    return body;
  }

  function show(view) {
    for (const id of ["viewSetup", "viewWork", "viewResult"]) $(id).hidden = id !== view;
    window.scrollTo({ top: 0, behavior: "smooth" });
  }

  // ───────── AI connection ─────────
  const KEY_HELP = {
    claude: 'Get a key at <a href="https://console.anthropic.com/settings/keys" target="_blank" rel="noopener">console.anthropic.com</a>. Usage is billed to your Anthropic account (a typical edit costs a few cents).',
    gemini: 'Get a free key at <a href="https://aistudio.google.com/apikey" target="_blank" rel="noopener">aistudio.google.com</a>. The free tier has daily limits.',
  };

  function renderAI() {
    const pill = $("aiPill");
    pill.classList.toggle("on", state.connected);
    $("aiPillText").textContent = state.connected
      ? (state.provider === "claude" ? "Claude connected" : "Gemini connected")
      : "Connect AI";
    $("aiForget").hidden = !state.connected;
    for (const b of $("provider").children) b.setAttribute("aria-checked", String(b.dataset.v === state.provider));
    $("keyHelp").innerHTML = KEY_HELP[state.provider];
    $("aiKey").placeholder = state.provider === "claude" ? "sk-ant-…" : "AIza…";
    updateGo();
    $("reviseHint").textContent = state.connected ? "" : "Connect your AI (top right) to edit by sentence.";
  }

  async function connect() {
    const key = $("aiKey").value.trim();
    const err = $("aiErr");
    err.hidden = true;
    if (!key) { err.textContent = "Paste your API key first."; err.hidden = false; return; }
    const btn = $("aiConnect");
    btn.disabled = true; btn.textContent = "Checking…";
    try {
      await api("/api/verify", { method: "POST", headers: { "X-AI-Provider": state.provider, "X-AI-Key": key } });
      state.key = key; state.connected = true;
      store.set("splice.provider", state.provider); store.set("splice.key", key);
      $("aiKey").value = "";
      $("aiDialog").close();
      toast("AI connected", "ok");
    } catch (e) {
      err.textContent = e.message; err.hidden = false;
    } finally {
      btn.disabled = false; btn.textContent = "Connect";
      renderAI();
    }
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
    } catch {
      state.connected = false; // key expired or backend unreachable; user can reconnect
    }
    renderAI();
  }

  // ───────── clips ─────────
  const VIDEO_RE = /\.(mp4|mov|m4v|mkv|webm)$/i;

  function addFiles(list) {
    let rejected = 0;
    for (const f of list) {
      const ok = f.type.startsWith("video/") || VIDEO_RE.test(f.name);
      if (!ok) { rejected++; continue; }
      if (state.clips.length >= state.limits.clips) { toast(`Up to ${state.limits.clips} clips per edit.`, "err"); break; }
      if (state.clips.some((c) => c.name === f.name && c.size === f.size)) continue;
      state.clips.push(f);
    }
    if (rejected) toast(`${rejected} file${rejected > 1 ? "s" : ""} skipped: only video files work here.`, "err");
    renderClips();
  }

  function renderClips() {
    const ul = $("clipList");
    ul.innerHTML = "";
    state.clips.forEach((f, i) => {
      const li = document.createElement("li");
      li.className = "clip";
      const url = URL.createObjectURL(f);
      li.innerHTML =
        `<video src="${url}#t=0.4" muted preload="metadata"></video>` +
        `<div><div class="nm">${esc(f.name)}</div><div class="sz">${fmtMB(f.size)}</div></div>` +
        `<button class="x" type="button" aria-label="Remove ${esc(f.name)}">×</button>`;
      li.querySelector("button").addEventListener("click", () => {
        URL.revokeObjectURL(url);
        state.clips.splice(i, 1);
        renderClips();
      });
      ul.appendChild(li);
    });
    const total = state.clips.reduce((a, f) => a + f.size, 0);
    $("clipMeta").textContent = state.clips.length
      ? `${state.clips.length} clip${state.clips.length > 1 ? "s" : ""} · ${fmtMB(total)}`
      : "";
    updateGo();
  }

  function updateGo() {
    const total = state.clips.reduce((a, f) => a + f.size, 0) + (state.music ? state.music.size : 0);
    const tooBig = total > state.limits.mb * 1048576;
    const go = $("go");
    go.disabled = state.busy || state.backendDown || state.clips.length === 0 || tooBig;
    $("goHint").textContent = state.clips.length === 0
      ? "Add at least one clip to begin."
      : tooBig
        ? `That's ${fmtMB(total)}; the limit is ${state.limits.mb} MB per edit.`
        : state.connected
          ? "Your AI will look at the footage and follow your brief."
          : "No AI connected: you'll get a clean automatic edit. Connect one for smarter cuts.";
  }

  // ───────── create + progress ─────────
  const STEP_BY_STAGE = [
    [/^Upload/i, "upload"], [/orientation/i, "orient"], [/^Analyzing/i, "analyze"],
    [/^Planning|^Applying/i, "plan"], [/^Rendering|^Done/i, "render"],
  ];
  const STEP_ORDER = ["upload", "orient", "analyze", "plan", "render"];

  function setStep(step, finished = false) {
    const idx = STEP_ORDER.indexOf(step);
    for (const li of $("steps").children) {
      const i = STEP_ORDER.indexOf(li.dataset.s);
      li.className = finished || i < idx ? "done" : i === idx ? "active" : "";
    }
  }
  function setProgress(frac) {
    const f = Math.max(0, Math.min(1, frac));
    $("ringBar").style.strokeDashoffset = String(326.7 * (1 - f));
    $("ringPct").textContent = Math.round(f * 100) + "%";
  }

  function upload(form) {
    return new Promise((resolve, reject) => {
      const xhr = new XMLHttpRequest();
      xhr.open("POST", API + "/api/jobs");
      for (const [k, v] of Object.entries(aiHeaders())) xhr.setRequestHeader(k, v);
      xhr.upload.onprogress = (e) => {
        if (e.lengthComputable) setProgress((e.loaded / e.total) * 0.2);
      };
      xhr.onload = () => {
        let body = {};
        try { body = JSON.parse(xhr.responseText); } catch { /* ignore */ }
        if (xhr.status >= 200 && xhr.status < 300) resolve(body);
        else reject(new Error(body.error || `Upload failed (${xhr.status})`));
      };
      xhr.onerror = () => reject(new Error("Couldn't reach the editor. Check your connection and try again."));
      xhr.send(form);
    });
  }

  async function start() {
    state.busy = true; updateGo();
    const form = new FormData();
    for (const f of state.clips) form.append("clips", f, f.name);
    if (state.music) form.append("music", state.music, state.music.name);
    form.append("prompt", $("prompt").value.trim());
    form.append("title", $("title").value.trim());
    form.append("preset", state.preset);
    form.append("duration", $("duration").value);

    show("viewWork");
    $("workTitle").textContent = "Uploading your footage";
    $("workSub").textContent = `Sending ${fmtMB(state.clips.reduce((a, f) => a + f.size, 0))}…`;
    setStep("upload"); setProgress(0);

    try {
      const { id } = await upload(form);
      state.job = { id, revision: 0 };
      await poll(id, 1);
      await showResult(id);
    } catch (e) {
      toast(e.message, "err");
      show("viewSetup");
    } finally {
      state.busy = false; updateGo();
    }
  }

  // Polls until the job is done at revision >= wantRev. Calls onTick(view).
  function poll(id, wantRev, onTick = reflectStage) {
    return new Promise((resolve, reject) => {
      let misses = 0;
      const tick = async () => {
        try {
          const v = await api(`/api/jobs/${id}`);
          misses = 0;
          if (onTick) onTick(v);
          if (v.status === "error") return reject(new Error(v.error || "The edit failed."));
          if (v.status === "done" && v.revision >= wantRev) return resolve(v);
          if (v.status === "done" && v.error) return reject(new Error(v.error));
        } catch (e) {
          if (++misses > 6) return reject(e);
        }
        setTimeout(tick, 700);
      };
      tick();
    });
  }

  function reflectStage(v) {
    const hit = STEP_BY_STAGE.find(([re]) => re.test(v.stage || ""));
    if (hit) setStep(hit[1]);
    $("workTitle").textContent = v.stage || "Working";
    $("workSub").textContent = v.status === "queued" ? "Waiting in line…" : "Hang tight, this usually takes a minute or two.";
    setProgress(0.2 + (v.progress || 0) * 0.8);
  }

  // ───────── result ─────────
  const PALETTE = ["#7c5cff", "#ff5c93", "#ffb35c", "#3ddc97", "#4cc3ff", "#c46bff", "#ff7a59", "#8bd450"];

  function renderTimeline(edl) {
    const cuts = (edl && edl.cuts) || [];
    const durs = cuts.map((c) => Math.max(0.1, (c.end - c.start) / (c.speed > 0 ? c.speed : 1)));
    const total = durs.reduce((a, b) => a + b, 0) || 1;
    const names = [];
    const tl = $("timeline");
    tl.innerHTML = "";
    cuts.forEach((c, i) => {
      const base = c.clip.split("/").pop();
      let ci = names.indexOf(base);
      if (ci < 0) { names.push(base); ci = names.length - 1; }
      const el = document.createElement("div");
      el.className = "seg-cut" + (c.caption ? " has-cap" : "") + (c.speed && c.speed !== 1 || c.zoom > 1 || c.transition ? " has-fx" : "");
      el.style.flex = String(durs[i] / total);
      el.style.background = PALETTE[ci % PALETTE.length];
      el.title = `${i + 1}. ${base}  ${durs[i].toFixed(1)}s` + (c.speed && c.speed !== 1 ? `  ${c.speed}×` : "") + (c.caption ? `\n“${c.caption}”` : "");
      tl.appendChild(el);
    });
    const lg = $("legend");
    lg.innerHTML = "";
    names.forEach((n, i) => {
      const li = document.createElement("li");
      const m = /^clip(\d+)/.exec(n);
      const label = m ? "Clip " + (parseInt(m[1], 10) + 1) : n.replace(/\.[^.]+$/, "");
      li.innerHTML = `<i style="background:${PALETTE[i % PALETTE.length]}"></i>${esc(label)}`;
      lg.appendChild(li);
    });
    $("tlMeta").textContent = `${cuts.length} cuts · ${total.toFixed(0)}s`;
  }

  function setVideo(id, rev) {
    const url = `${API}/api/jobs/${id}/video?r=${rev}`;
    const v = $("player");
    v.src = url;
    v.load();
    $("download").href = url;
  }

  async function showResult(id, v) {
    v = v || (await api(`/api/jobs/${id}`));
    state.job = { id, revision: v.revision };
    $("playerWrap").style.aspectRatio = state.preset.replace(":", " / ");
    setVideo(id, v.revision);
    renderTimeline(v.edl);
    const fixes = v.edl && v.edl.fixes ? Object.keys(v.edl.fixes).length : 0;
    $("resultMeta").innerHTML =
      `<span class="pill">${v.planner === "rule-based" ? "Automatic edit" : "Edited by " + esc(v.planner)}</span>` +
      (fixes ? `<span class="pill">${fixes} clip${fixes > 1 ? "s" : ""} rotated upright</span>` : "") +
      `<span class="pill">${esc(state.preset)}</span>`;
    show("viewResult");
    $("player").focus({ preventScroll: true });
  }

  async function revise(instruction) {
    if (!state.job) return;
    if (!state.connected) {
      toast("Connect your AI first to edit by sentence.", "err");
      $("aiDialog").showModal();
      return;
    }
    const input = $("reviseInput"), go = $("reviseGo");
    input.disabled = go.disabled = true;
    $("playerBusy").hidden = false;
    $("busyText").textContent = "Applying your change…";
    const want = state.job.revision + 1;
    try {
      await api(`/api/jobs/${state.job.id}/revise`, {
        method: "POST",
        headers: { "Content-Type": "application/json", ...aiHeaders() },
        body: JSON.stringify({ instruction }),
      });
      const v = await poll(state.job.id, want, (t) => {
        if (t.stage) $("busyText").textContent = t.stage + "…";
      });
      await showResult(state.job.id, v);
      input.value = "";
      toast("Done: your change is in.", "ok");
    } catch (e) {
      toast(e.message, "err");
    } finally {
      input.disabled = go.disabled = false;
      $("playerBusy").hidden = true;
    }
  }

  // ───────── wiring ─────────
  function init() {
    // drop zone
    const drop = $("drop"), input = $("fileInput");
    drop.addEventListener("click", () => input.click());
    drop.addEventListener("keydown", (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); input.click(); } });
    input.addEventListener("change", () => { addFiles(input.files); input.value = ""; });
    for (const ev of ["dragenter", "dragover"]) drop.addEventListener(ev, (e) => { e.preventDefault(); drop.classList.add("over"); });
    for (const ev of ["dragleave", "drop"]) drop.addEventListener(ev, (e) => { e.preventDefault(); drop.classList.remove("over"); });
    drop.addEventListener("drop", (e) => addFiles(e.dataTransfer.files));
    // allow dropping anywhere on the page
    window.addEventListener("dragover", (e) => e.preventDefault());
    window.addEventListener("drop", (e) => { e.preventDefault(); if (!$("viewSetup").hidden && e.dataTransfer.files.length) addFiles(e.dataTransfer.files); });

    // brief
    $("ideas").addEventListener("click", (e) => {
      if (e.target.tagName === "BUTTON") { $("prompt").value = e.target.textContent; $("prompt").focus(); }
    });
    $("duration").addEventListener("input", (e) => { $("durOut").textContent = e.target.value + "s"; });
    $("presets").addEventListener("click", (e) => {
      const b = e.target.closest("button"); if (!b) return;
      state.preset = b.dataset.v;
      for (const x of $("presets").children) x.setAttribute("aria-checked", String(x === b));
    });

    // music
    $("musicInput").addEventListener("change", (e) => {
      const f = e.target.files[0];
      state.music = f || null;
      $("musicText").textContent = f ? f.name : "Choose a track…";
      $("musicLabel").classList.toggle("has", !!f);
      $("musicClear").hidden = !f;
      updateGo();
    });
    $("musicClear").addEventListener("click", (e) => {
      e.preventDefault(); e.stopPropagation();
      state.music = null; $("musicInput").value = "";
      $("musicText").textContent = "Choose a track…";
      $("musicLabel").classList.remove("has"); $("musicClear").hidden = true;
      updateGo();
    });

    $("go").addEventListener("click", start);

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

    // result actions
    $("again").addEventListener("click", () => {
      state.clips = []; state.job = null; renderClips();
      $("player").removeAttribute("src");
      show("viewSetup");
    });
    $("reviseForm").addEventListener("submit", (e) => {
      e.preventDefault();
      const t = $("reviseInput").value.trim();
      if (t) revise(t);
    });
    $("reviseIdeas").addEventListener("click", (e) => {
      if (e.target.tagName === "BUTTON") { $("reviseInput").value = e.target.textContent; $("reviseInput").focus(); }
    });

    // limits from the backend
    api("/api/health").then((h) => {
      if (h.max_clips) state.limits.clips = h.max_clips;
      if (h.max_upload_mb) state.limits.mb = h.max_upload_mb;
      if (h.server_ai && !state.key) { state.connected = true; state.provider = "claude"; }
      renderAI();
    }).catch(() => {
      state.backendDown = true;
      const b = $("banner");
      b.innerHTML = "<b>Preview mode:</b> the editing backend isn't connected to this page yet, so edits can't run. " +
        "The site owner needs to point <code>SPLICE_API</code> at the running backend (see the README).";
      b.hidden = false;
      $("go").disabled = true;
    });

    renderAI();
    renderClips();
    restoreConnection();
  }

  init();
})();
