(() => {
  "use strict";

  const API = (window.SPLICE_API || "").replace(/\/$/, "");
  const $ = (id) => document.getElementById(id);
  const store = {
    get(k) { try { return localStorage.getItem(k); } catch { return null; } },
    set(k, v) { try { localStorage.setItem(k, v); } catch { /* private mode */ } },
    del(k) { try { localStorage.removeItem(k); } catch { /* ignore */ } },
  };

  // Anonymous, random, browser-local id: only unlocks this browser's saved
  // settings and its own recent edits. Not a login and not a secret.
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
    clips: [],            // File[]
    music: null,          // File | null
    preset: "9:16",
    briefMode: "describe", // "describe" | "reference"
    ref: null,             // File | null (reference video)
    provider: store.get("splice.provider") || "claude",
    key: store.get("splice.key") || "",
    connected: false,
    limits: { clips: 200, mb: 100000 },
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
    opts = { ...opts, headers: { "X-Visitor": visitor, ...(opts.headers || {}) } };
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
      : "AI: optional";
    $("aiForget").hidden = !state.connected;
    for (const b of $("provider").children) b.setAttribute("aria-checked", String(b.dataset.v === state.provider));
    $("keyHelp").innerHTML = KEY_HELP[state.provider];
    $("aiKey").placeholder = state.provider === "claude" ? "sk-ant-…" : "AIza…";
    updateGo();
    $("reviseHint").textContent = state.connected
      ? "AI is connected, so you can ask for anything."
      : "Works without AI for common changes (speed, mute, remove, titles, zoom…). Connect AI for open-ended requests.";
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

  // ───────── app updates (local app only) ─────────
  async function checkForUpdate() {
    try {
      const u = await api("/api/update");
      if (u.version && u.version !== "dev") $("footVersion").textContent = " · " + u.version;
      if (!u.enabled || !u.available) { $("updatePill").hidden = true; return; }
      $("updateText").textContent = `Update ${u.latest} ready`;
      $("updatePill").title = u.notes || "";
      $("updatePill").hidden = false;
    } catch { /* not the local app, or offline: nothing to show */ }
  }

  async function applyUpdate() {
    const pill = $("updatePill");
    pill.disabled = true;
    $("updateText").textContent = "Updating…";
    try {
      const r = await api("/api/update/apply", { method: "POST" });
      $("updateText").textContent = "Restarting…";
      // Wait for the new version to come back, then reload onto it.
      const t0 = Date.now();
      for (;;) {
        await new Promise((res) => setTimeout(res, 700));
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

  // ── chunked, parallel, resumable upload ──
  const PARALLEL = 4;
  const fmtRate = (bps) => (bps >= 1048576 ? (bps / 1048576).toFixed(1) + " MB/s" : Math.round(bps / 1024) + " KB/s");
  const fmtETA = (s) => (!isFinite(s) ? "…" : s < 60 ? Math.ceil(s) + "s" : Math.ceil(s / 60) + " min");

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

  // items: [{ file, kind: "clip"|"inspo"|"music", index }]
  async function uploadAll(items) {
    const sess = await api("/api/uploads", { method: "POST" });
    const chunk = sess.chunk_bytes || 32 * 1048576;
    const tasks = [];
    for (const it of items) {
      for (let off = 0; off < it.file.size; off += chunk) tasks.push({ it, off });
    }
    const total = items.reduce((a, it) => a + it.file.size, 0);
    const inflight = new Map(); // task -> bytes sent for the chunk in flight
    let done = 0;
    let retrying = 0;
    const t0 = Date.now();

    const render = () => {
      let sent = done;
      for (const v of inflight.values()) sent += v;
      const secs = Math.max(1, (Date.now() - t0) / 1000);
      const rate = sent / secs;
      setProgress(sent / total); // the ring is the upload itself while uploading
      $("workSub").textContent = `${fmtMB(sent)} of ${fmtMB(total)}`;
      let line = "";
      if (retrying > 0) {
        line = "Connection hiccup, retrying…";
      } else if (sent === 0) {
        line = secs < 8 ? "Connecting…" : "Still waiting for the connection to start moving data…";
      } else {
        const eta = (total - sent) / Math.max(1, rate);
        line = `${fmtRate(rate)} · about ${fmtETA(eta)} left`;
        if (secs > 6 && eta > 20 * 60) {
          line += ` — that's a long upload for this connection. Fewer or shorter clips will be much faster.`;
        }
      }
      $("upSpeed").textContent = line;
    };
    const tick = setInterval(render, 500);
    const report = () => {};
    render();

    let next = 0;
    const worker = async () => {
      while (next < tasks.length) {
        const task = tasks[next++];
        const { it, off } = task;
        const blob = it.file.slice(off, Math.min(it.file.size, off + chunk));
        const headers = {
          "X-Visitor": visitor,
          "X-Kind": it.kind, "X-Index": String(it.index), "X-Name": it.file.name,
          "X-Offset": String(off), "X-Total": String(it.file.size),
        };
        let attempt = 0;
        for (;;) {
          try {
            inflight.set(task, 0);
            await putChunk(`${API}/api/uploads/${sess.upload_id}/chunk`, headers, blob, (n) => { inflight.set(task, n); report(); });
            inflight.delete(task);
            done += blob.size;
            report();
            break;
          } catch (e) {
            inflight.delete(task);
            if (e.status === 404) throw new Error("The editor restarted while you were uploading. Please press Create my edit again.");
            if (e.fatal || ++attempt > 5) throw e;
            retrying++;
            await new Promise((r) => setTimeout(r, 600 * 2 ** attempt)); // back off, then resend this chunk
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
    $("upSpeed").textContent = "";
    return sess.upload_id;
  }

  // You are on the same network as the editor but going the long way round
  // (out to the internet and back): point at the direct address.
  function showSameNetworkBanner(urls) {
    const b = $("banner");
    b.hidden = false;
    b.style.borderColor = "rgba(61, 220, 151, 0.4)";
    b.style.background = "rgba(61, 220, 151, 0.08)";
    b.style.color = "#c9f5df";
    const links = urls.map((u, i) =>
      `<a href="${u}" style="color:#fff;font-weight:700">${i === 0 ? "on this computer" : "from another device on your Wi-Fi"}: ${u.replace("http://", "")}</a>`).join(" &nbsp;·&nbsp; ");
    b.innerHTML = `<b>Faster way:</b> you're on the same network as the editor, so uploading through this link goes out to the internet and back. Use the direct address instead (uploads are instant): ${links}`;
  }

  // Time a real 1 MB upload to the editor so a slow path is reported up front.
  async function probeSpeed() {
    const blob = new Blob([new Uint8Array(1 << 20)]);
    const t = performance.now();
    const res = await fetch(API + "/api/ping", { method: "POST", body: blob, headers: { "X-Visitor": visitor } });
    if (!res.ok) throw new Error("The editor didn't answer the connection check.");
    await res.arrayBuffer();
    return blob.size / Math.max(0.05, (performance.now() - t) / 1000); // bytes/sec
  }

  function askAboutSlowUpload(bytes, rate) {
    const dlg = $("slowDialog");
    const mins = bytes / rate / 60;
    const eta = mins >= 90 ? (mins / 60).toFixed(1) + " hours" : Math.round(mins) + " minutes";
    $("slowBody").textContent =
      `Your connection to the editor runs at about ${fmtRate(rate)}. Uploading ${fmtMB(bytes)} would take roughly ${eta}.`;
    const tips = [
      "<b>You rarely need that much.</b> A 30-second video usually only needs a few minutes of footage: try fewer or shorter clips.",
      "<b>Trim first.</b> Cut clips down in your phone's photo app before uploading.",
    ];
    if (state.localUrls && state.localUrls.length) {
      tips.unshift("<b>You're on the same network as the editor.</b> Use the direct address and this takes seconds: " +
        state.localUrls.map((u) => `<a href="${u}" style="color:#fff">${u.replace("http://", "")}</a>`).join(" or "));
    } else if (!/^(localhost|127\.0\.0\.1)$/.test(location.hostname)) {
      tips.push("<b>Testing from the same computer that runs the editor?</b> Open <code>http://localhost:7860</code> instead of this link. Going through the internet is what's slow.");
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

  async function start() {
    state.busy = true; updateGo();
    try {
      await startInner();
    } catch (e) {
      // Whatever goes wrong, never leave the user staring at 0%.
      console.error(e);
      toast((e && e.message) || "Something went wrong starting the edit.", "err");
      show("viewSetup");
    } finally {
      state.busy = false; updateGo();
    }
  }

  async function startInner() {
    const items = state.clips.map((file, index) => ({ file, kind: "clip", index }));
    if (state.music) items.push({ file: state.music, kind: "music", index: 0 });
    const useRef = state.briefMode === "reference";
    if (useRef && state.ref) items.push({ file: state.ref, kind: "inspo", index: 0 });
    const refUrl = useRef && !state.ref ? $("refUrl").value.trim() : "";

    saveMemory();
    show("viewWork");
    $("workTitle").textContent = "Checking your connection";
    $("workSub").textContent = "A quick speed test before sending anything…";
    setStep("upload"); setProgress(0);
    const totalBytes = items.reduce((a, it) => a + it.file.size, 0);
    if (totalBytes > 100 * 1048576) {
      try {
        const rate = await probeSpeed();
        if (totalBytes / rate > 10 * 60 && !(await askAboutSlowUpload(totalBytes, rate))) {
          show("viewSetup");
          return;
        }
      } catch (e) {
        toast(e.message, "err");
        show("viewSetup");
        return;
      }
    }
    $("workTitle").textContent = "Uploading your footage";
    $("workSub").textContent = `Sending ${fmtMB(items.reduce((a, it) => a + it.file.size, 0))}…`;
    $("upSpeed").textContent = "";
    setStep("upload"); setProgress(0);

    try {
      const uploadId = await uploadAll(items);
      $("workTitle").textContent = "Starting your edit";
      const { id } = await api("/api/jobs", {
        method: "POST",
        headers: { "Content-Type": "application/json", ...aiHeaders() },
        body: JSON.stringify({
          upload_id: uploadId,
          prompt: $("prompt").value.trim(),
          title: $("title").value.trim(),
          preset: state.preset,
          duration: $("duration").value,
          inspo_url: refUrl,
        }),
      });
      state.job = { id, revision: 0 };
      await poll(id, 1);
      await showResult(id);
    } catch (e) {
      toast(e.message, "err");
      show("viewSetup");
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
    setProgress(v.progress || 0);
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
    $("undoBtn").disabled = !v.can_undo;
    setVideo(id, v.revision);
    renderTimeline(v.edl);
    const fixes = v.edl && v.edl.fixes ? Object.keys(v.edl.fixes).length : 0;
    $("resultMeta").innerHTML =
      `<span class="pill">${v.planner === "rule-based" ? "Automatic edit" : "Edited by " + esc(v.planner)}</span>` +
      (fixes ? `<span class="pill">${fixes} clip${fixes > 1 ? "s" : ""} rotated upright</span>` : "") +
      `<span class="pill">${esc(state.preset)}</span>`;
    const notes = $("resultNotes");
    notes.innerHTML = "";
    for (const n of v.notes || []) {
      const li = document.createElement("li");
      li.textContent = n;
      notes.appendChild(li);
    }
    show("viewResult");
    $("player").focus({ preventScroll: true });
  }

  // ── memory: saved preferences + recent edits ──
  function saveMemory() {
    const prefs = {
      preset: state.preset, duration: $("duration").value, briefMode: state.briefMode,
      prompt: $("prompt").value.trim().slice(0, 500), title: $("title").value.trim().slice(0, 80),
    };
    api("/api/me/prefs", { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify(prefs) })
      .catch(() => { /* memory is a nicety; never block an edit on it */ });
  }

  function applyPrefs(p) {
    if (!p) return;
    if (p.preset && schemaPresets.includes(p.preset)) {
      state.preset = p.preset;
      for (const b of $("presets").children) b.setAttribute("aria-checked", String(b.dataset.v === p.preset));
    }
    if (p.duration && +p.duration >= 10 && +p.duration <= 90) {
      $("duration").value = p.duration;
      $("durOut").textContent = p.duration + "s";
    }
    if (p.title && !$("title").value) $("title").value = p.title;
    if (p.briefMode === "reference") $("briefMode").querySelector('[data-v="reference"]').click();
  }
  const schemaPresets = ["9:16", "1:1", "4:5", "16:9"];

  function renderRecent(list) {
    const ul = $("recentList");
    ul.innerHTML = "";
    const usable = (list || []).filter((r) => r.playable);
    $("recent").hidden = usable.length === 0;
    for (const r of usable.slice(0, 5)) {
      const li = document.createElement("li");
      const when = new Date(r.created).toLocaleString([], { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" });
      li.innerHTML = `<button type="button"><b>${esc(r.title || "Untitled edit")}</b><small>${esc(when)} · ${esc(r.preset || "")}</small></button>`;
      li.querySelector("button").addEventListener("click", async () => {
        try {
          if (r.preset) state.preset = r.preset;
          await showResult(r.id);
        } catch (e) { toast("That edit is no longer available.", "err"); }
      });
      ul.appendChild(li);
    }
  }

  async function undo() {
    if (!state.job) return;
    const btn = $("undoBtn");
    btn.disabled = true;
    $("playerBusy").hidden = false;
    $("busyText").textContent = "Undoing…";
    const want = state.job.revision + 1;
    try {
      await api(`/api/jobs/${state.job.id}/undo`, { method: "POST" });
      const v = await poll(state.job.id, want, (t) => { if (t.stage) $("busyText").textContent = t.stage + "…"; });
      await showResult(state.job.id, v);
      toast("Undone.", "ok");
    } catch (e) {
      toast(e.message, "err");
    } finally {
      $("playerBusy").hidden = true;
    }
  }

  async function revise(instruction) {
    if (!state.job) return;
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

    // vibe: describe vs match a video
    $("briefMode").addEventListener("click", (e) => {
      const b = e.target.closest("button"); if (!b) return;
      state.briefMode = b.dataset.v;
      for (const x of $("briefMode").children) x.setAttribute("aria-checked", String(x === b));
      const ref = state.briefMode === "reference";
      $("refBlock").hidden = !ref;
      $("ideas").hidden = ref;
      $("promptLabel").textContent = ref ? "Anything to add? (optional)" : "What should this video feel like?";
      $("prompt").placeholder = ref ? "e.g. but keep it a bit calmer" : "e.g. Fast-paced travel reel, open on the best view, keep it upbeat";
    });
    $("refFile").addEventListener("change", (e) => {
      const f = e.target.files[0];
      state.ref = f || null;
      $("refText").textContent = f ? f.name : "Upload a reference video…";
      $("refLabel").classList.toggle("has", !!f);
      $("refClear").hidden = !f;
      if (f) $("refUrl").value = "";
    });
    $("refClear").addEventListener("click", (e) => {
      e.preventDefault(); e.stopPropagation();
      state.ref = null; $("refFile").value = "";
      $("refText").textContent = "Upload a reference video…";
      $("refLabel").classList.remove("has"); $("refClear").hidden = true;
    });
    $("refUrl").addEventListener("input", () => {
      if ($("refUrl").value.trim() && state.ref) $("refClear").click();
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

    // result actions
    $("again").addEventListener("click", () => {
      state.clips = []; state.job = null; renderClips();
      $("player").removeAttribute("src");
      show("viewSetup");
    });
    $("undoBtn").addEventListener("click", undo);
    $("updatePill").addEventListener("click", applyUpdate);
    checkForUpdate();
    setInterval(checkForUpdate, 15 * 60 * 1000);
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
      if (h.max_upload_mb) {
        state.limits.mb = h.max_upload_mb;
        $("limitText").textContent = ` · up to ${h.max_upload_mb.toLocaleString()} MB per edit`;
      }
      if (h.server_ai && !state.key) { state.connected = true; state.provider = "claude"; }
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
    api("/api/me").then((me) => { applyPrefs(me.prefs); renderRecent(me.recent); }).catch(() => {});
  }

  window.addEventListener("unhandledrejection", (e) => {
    console.error(e.reason);
    if (!$("viewWork").hidden && state.busy === false) return;
    toast((e.reason && e.reason.message) || "Something went wrong.", "err");
  });

  init();
})();
