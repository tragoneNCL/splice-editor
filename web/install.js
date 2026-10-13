(() => {
  "use strict";
  const REPO = window.SPLICE_REPO || "tragoneNCL/splice-editor";
  const $ = (id) => document.getElementById(id);
  const RAW = `https://raw.githubusercontent.com/${REPO}/main`;
  const CMDS = {
    unix: `curl -fsSL ${RAW}/install.sh | sh`,
    win: `irm ${RAW}/install.ps1 | iex`,
  };

  function detect() {
    const ua = navigator.userAgent || "";
    const plat = (navigator.userAgentData && navigator.userAgentData.platform) || navigator.platform || "";
    if (/Win/i.test(plat) || /Windows/i.test(ua)) return { os: "windows", arch: "amd64", name: "Windows" };
    if (/Mac/i.test(plat) || /Macintosh/i.test(ua)) {
      // Browsers hide Apple Silicon; offer both and default to arm64 (all Macs since 2020).
      return { os: "darwin", arch: "arm64", name: "macOS" };
    }
    if (/Linux/i.test(plat) || /X11|Linux/i.test(ua)) return { os: "linux", arch: "amd64", name: "Linux" };
    return null;
  }

  const LABEL = {
    "darwin-arm64": "macOS · Apple Silicon (M1 and newer)",
    "darwin-amd64": "macOS · Intel",
    "windows-amd64": "Windows · 64-bit",
    "linux-amd64": "Linux · 64-bit",
  };

  function fmtMB(b) { return (b / 1048576).toFixed(1) + " MB"; }

  async function load() {
    const me = detect();
    $("detected").textContent = me ? `Detected: ${me.name}` : "Choose your system below";
    const tab = me && me.os === "windows" ? "win" : "unix";
    setTab(tab);

    let rel = null;
    try {
      const r = await fetch(`https://api.github.com/repos/${REPO}/releases/latest`, { headers: { Accept: "application/vnd.github+json" } });
      if (r.ok) rel = await r.json();
    } catch { /* offline or rate limited */ }

    const list = $("dlList");
    list.innerHTML = "";
    if (!rel || !rel.assets) {
      $("noRelease").hidden = false;
      $("primaryBtn").textContent = "First release coming soon";
      $("primaryNote").textContent = "You can already use Splice in the browser from the app that's running on your computer.";
      return;
    }
    $("relMeta").textContent = rel.tag_name;
    $("ver").textContent = rel.tag_name;
    const assets = rel.assets.filter((a) => /^splice-.*\.(tar\.gz|zip)$/.test(a.name));
    let primary = null;
    for (const a of assets) {
      const m = /-(darwin|linux|windows)-(amd64|arm64)\./.exec(a.name);
      if (!m) continue;
      const key = `${m[1]}-${m[2]}`;
      const li = document.createElement("li");
      li.innerHTML = `<span><b>${LABEL[key] || key}</b><small>${a.name} · ${fmtMB(a.size)}</small></span>`;
      const link = document.createElement("a");
      link.className = "btn";
      link.href = a.browser_download_url;
      link.textContent = "Download";
      li.appendChild(link);
      list.appendChild(li);
      if (me && key === `${me.os}-${me.arch}`) primary = a;
    }
    if (primary) {
      $("primaryBtn").href = primary.browser_download_url;
      $("primaryBtn").textContent = `Download for ${me.name}`;
      $("primaryBtn").removeAttribute("aria-disabled");
      $("primaryNote").textContent = `${rel.tag_name} · ${fmtMB(primary.size)}${me.os === "darwin" ? " · Intel Mac? Pick the Intel build below." : ""}`;
    } else {
      $("primaryBtn").textContent = "Choose a download below";
    }
  }

  function setTab(v) {
    for (const b of $("osTabs").children) b.setAttribute("aria-selected", String(b.dataset.v === v));
    $("cmdText").textContent = CMDS[v];
    $("cmdHelp").textContent = v === "win"
      ? "Open PowerShell (not Command Prompt), paste, press Enter."
      : "Open Terminal, paste, press Enter. Nothing needs sudo.";
  }

  $("osTabs").addEventListener("click", (e) => { const b = e.target.closest("button"); if (b) setTab(b.dataset.v); });
  $("copyCmd").addEventListener("click", async () => {
    try { await navigator.clipboard.writeText($("cmdText").textContent); $("copyCmd").textContent = "Copied"; }
    catch { $("copyCmd").textContent = "Select and copy the command above"; }
    setTimeout(() => ($("copyCmd").textContent = "Copy command"), 2000);
  });

  let deferred = null;
  window.addEventListener("beforeinstallprompt", (e) => { e.preventDefault(); deferred = e; $("pwaCard").hidden = false; });
  $("pwaInstall").addEventListener("click", async () => { if (deferred) { deferred.prompt(); deferred = null; $("pwaCard").hidden = true; } });

  load();
})();
