# Splice

Hands-off video editing. Drop in clips and a sentence; get back a finished edit.

- **Fixes orientation** — phone rotation metadata is applied deterministically, and a vision check catches clips stored upside-down with no metadata.
- **Picks the moments** — scene detection + quality scoring, then an AI planner chooses a strong opening and cuts to your brief.
- **Real editing features** — speed, volume, transitions, titles/captions, music with ducking, zoom/crop, color, fades, export presets.
- **Remembers** — edits, their undo history and each visitor's settings persist in SQLite across restarts; **Undo** steps back one change.
- **Edit by sentence** — "make the second shot slow motion" revises the same project file and re-renders.
- **Match a reference video** — paste a YouTube link (needs `yt-dlp`) or upload one; only its pacing is measured.
- **Bring your own AI key** — Claude or Gemini (free tier). Keys stay in the visitor's browser and are never stored server-side.

The edit decision list (EDL) is the project file: the AI writes it, the renderer plays it, and people or chat commands edit the same document.

## Run it locally

Requires Go 1.27+ and `ffmpeg` on your PATH.

```bash
go run ./cmd/server          # http://localhost:7860  (UI + API)
go test ./...                # fast tests; synthetic footage, no AI needed
```

Command-line tools:

```bash
go run ./cmd/synth -out ./testfootage            # sample clips incl. upside-down + phone-rotated
go run ./cmd/auto -clips ./testfootage -prompt "calm and simple" -out final.mp4
go run ./cmd/revise -edl edit.json -do "slow-mo the second shot"
FILMEDITOR_LIVE=1 go test ./internal/e2e -v      # live model checks (uses your claude login)
```

## Install

- **Website:** open `/install` on any running copy (or `web/install.html`). It detects your system and offers the download plus a one-line installer.
- **Mac / Linux:** `curl -fsSL https://raw.githubusercontent.com/tragoneNCL/splice-editor/main/install.sh | sh`
- **Windows (PowerShell):** `irm https://raw.githubusercontent.com/tragoneNCL/splice-editor/main/install.ps1 | iex`

Both verify the download's checksum, install ffmpeg if it's missing, and need no administrator rights. The app updates itself afterwards. It is also an installable web app (your browser's "Install" button).

## Accounts, projects and library

Everything is stored on the computer running the app: accounts (email + password, bcrypt-hashed), a footage library (upload once, reuse), and projects with full history and undo. Set `REQUIRE_AUTH=0` to run without accounts, or `CLOSED_SIGNUP=1` to allow only the first account.

## Fastest: run it as a local app

Everything runs on your own computer, so there is no upload at all: a 1.8 GB
set of clips is edited in about 25 seconds. Download the app for your system
from the Releases page (one file; the UI is inside it), install ffmpeg once
(`brew install ffmpeg` on Mac, `winget install Gyan.FFmpeg` on Windows), then:

```bash
./splice -open        # opens http://localhost:7860 in your browser
```

Footage never leaves the machine. The app is same-origin only by default, so
other websites cannot drive it from your browser.

Uploading to a hosted copy is always limited by the uploader's own internet
connection; a local app has no such limit.

## Run it for a friend (from your own machine)

`scripts/live.sh` builds the server, starts a free Cloudflare quick tunnel, prints the public URL and points the GitHub Pages site at it. Put optional settings (e.g. a server-side `GEMINI_API_KEY`) in `~/.splice.env`, which is never committed. The link works only while the machine is awake and the script is running.

## Deploy

The site has two parts. **GitHub Pages can only host static files**, so the editing backend (Go + ffmpeg) runs somewhere that can run containers; the Pages site calls it.

1. **Backend** — build the included `Dockerfile` on any container host:
   - **Hugging Face Spaces** (free, generous RAM): new Space → SDK *Docker* → push this repo; use `deploy/huggingface/README.md` as the Space's `README.md`.
   - **Render** (free tier, sleeps when idle): New → Blueprint → select this repo (`render.yaml`).
   - Anywhere else: `docker build -t splice . && docker run -p 7860:7860 splice`.
2. **Front end** — enable Pages (Settings → Pages → Source: GitHub Actions), then set the repository **variable** `SPLICE_API` to the backend URL and re-run the *Pages* workflow.

The backend also serves the UI itself, so its own URL works standalone with no Pages step.

### Configuration (environment variables)

| Variable | Default | Purpose |
|---|---|---|
| `PORT` | `7860` | listen port |
| `DATA_DIR` | temp dir | uploads/results (auto-deleted after `TTL_MINUTES`) |
| `ALLOWED_ORIGIN` | `*` | CORS origin; set to your Pages URL |
| `GEMINI_API_KEY` | – | optional server-side key so visitors can skip bringing one |
| `GEMINI_MODEL` / `ANTHROPIC_MODEL` | see code | model overrides |
| `MAX_UPLOAD_MB`, `MAX_CLIPS`, `WORKERS`, `RATE_PER_HOUR`, `TTL_MINUTES` | 100000, 200, 1, 6, 60 | limits (uploads are chunked, parallel and resumable, so large files work behind proxies) |

## Layout

```
cmd/server    HTTP API + embedded UI        internal/pipeline  the whole edit as one call
cmd/auto      one-shot CLI                  internal/understand shots + quality scoring
cmd/revise    edit-by-sentence CLI          internal/orient    upside-down safety net
cmd/synth     test footage generator        internal/plan      rule-based + LLM planners
web/          static front end              internal/render    ffmpeg graph: speed, xfade, text, music…
                                            internal/project   undo/redo editing ops
                                            internal/llm       Claude (SDK) / Gemini / claude CLI
```

## Privacy and limits

Uploaded clips are stored only for the job and deleted automatically. Visitors' API keys travel only in request headers for their own jobs and are never logged or persisted. Bounded by upload size, clip count, queue length and a per-IP rate limit.
