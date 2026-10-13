#!/usr/bin/env bash
# Run Splice on this machine and expose it with a free Cloudflare quick tunnel.
#
#   scripts/live.sh
#
# Optional settings go in ~/.splice.env (KEY=VALUE lines, never committed):
#   GEMINI_API_KEY=...    server-side AI so visitors need no key of their own
#   ALLOWED_ORIGIN=https://<you>.github.io
#   MAX_UPLOAD_MB=100000  RATE_PER_HOUR=30  WORKERS=2
#
# When the tunnel URL changes it updates the SPLICE_API repo variable and
# redeploys the Pages site (needs `gh` logged in).
set -euo pipefail
cd "$(dirname "$0")/.."

if [ -f "$HOME/.splice.env" ]; then
  set -a; . "$HOME/.splice.env"; set +a
fi
: "${PORT:=7860}"
: "${DATA_DIR:=$HOME/.splice-data}"
: "${ALLOWED_ORIGIN:=*}"
export PORT DATA_DIR ALLOWED_ORIGIN

for bin in go ffmpeg ffprobe cloudflared; do
  command -v "$bin" >/dev/null || { echo "missing: $bin"; exit 1; }
done
command -v yt-dlp >/dev/null || echo "note: yt-dlp not found, YouTube reference links will be unavailable"

mkdir -p bin "$DATA_DIR"
go build -o bin/splice ./cmd/server
LOG="$DATA_DIR/server.log"; TLOG="$DATA_DIR/tunnel.log"
pkill -f "bin/splice" 2>/dev/null || true
pkill -f "cloudflared tunnel --url http://localhost:$PORT" 2>/dev/null || true
sleep 1

nohup ./bin/splice >"$LOG" 2>&1 &
SERVER_PID=$!
nohup cloudflared tunnel --url "http://localhost:$PORT" --no-autoupdate >"$TLOG" 2>&1 &

URL=""
for _ in $(seq 1 40); do
  URL=$(grep -o 'https://[a-z0-9-]*\.trycloudflare\.com' "$TLOG" | head -1 || true)
  [ -n "$URL" ] && break
  sleep 1
done
[ -n "$URL" ] || { echo "tunnel did not start; see $TLOG"; exit 1; }

echo "Splice is live: $URL"
echo "Server log: $LOG"

if command -v gh >/dev/null && gh auth status >/dev/null 2>&1; then
  REPO=$(git remote get-url origin 2>/dev/null | sed -E 's#.*github.com[:/](.*)(\.git)?$#\1#; s#\.git$##') || true
  if [ -n "${REPO:-}" ]; then
    CUR=$(gh variable get SPLICE_API --repo "$REPO" 2>/dev/null || true)
    if [ "$CUR" != "$URL" ]; then
      gh variable set SPLICE_API --body "$URL" --repo "$REPO" && gh workflow run pages.yml --repo "$REPO" \
        && echo "Pages site pointed at the new URL."
    fi
  fi
fi

# Keep the machine awake while the server runs.
exec caffeinate -i -w "$SERVER_PID"
