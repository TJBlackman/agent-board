#!/usr/bin/env bash
# End to end exercise of the CLI helper against a throwaway local server.
# Usage (WSL): bash scripts/e2e-local.sh
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
T=/tmp/ab-e2e
rm -rf "$T" && mkdir -p "$T/home" "$T/sub"
cd "$ROOT" && CGO_ENABLED=0 go build -o "$T/agent-board" ./cmd/agent-board
export AGENT_BOARD_DATA_DIR="$T/data" AGENT_BOARD_LISTEN=127.0.0.1:18080 AGENT_BOARD_ADMIN_LISTEN=127.0.0.1:18081
export AGENT_BOARD_ADMIN_URL=http://127.0.0.1:18081 AGENT_BOARD_HOME="$T/home" AGENT_BOARD_URL=http://127.0.0.1:18080
export AGENT_BOARD_SESSION_ID=e2e-session-1 AGENT_BOARD_BACKUP_DIR="$T/backups"
AB="$T/agent-board"
"$AB" serve > "$T/server.log" 2>&1 &
SP=$!
trap 'kill $SP 2>/dev/null || true' EXIT
sleep 1.5
echo "--- token ---"
"$AB" token create --label e2e --permission write > "$T/home/token"
wc -c < "$T/home/token"
echo "--- init ---"
"$AB" init --launch-dir /home/dev/proj/e2e --workspace e2e-ws --client codex
echo "--- post ---"
printf 'Observed permission denied on /data after restart.\n\nFix: chown 568:568 on the volume root. Verified after a restart.' | \
  "$AB" post --title "Docker volume permission denied after restart" --description "Startup failure and the ownership change that fixed it" \
  --category infrastructure --tags docker,permissions --scope demo/e2e --kind finding | tee "$T/post.json"
PID=$(sed -n 's/.*"post_id": *"\([^"]*\)".*/\1/p' "$T/post.json")
echo "post id: $PID"
echo "--- search ---"
"$AB" search 'permission denied' --scope demo/e2e
echo "--- recent ---"
"$AB" recent --limit 5
echo "--- taxonomy ---"
"$AB" taxonomy
echo "--- comment ---"
"$AB" comment "$PID" --body "Also reproduced with gitea; same chown fix."
echo "--- read ---"
"$AB" read "$PID"
echo "--- stats after one open ---"
"$AB" stats "$PID"
echo "--- comment from a subdirectory (new context) ---"
cd "$T/sub" && "$AB" comment "$PID" --body "from a subdirectory"
echo "--- whoami ---"
"$AB" whoami
echo "--- search comment phrase ---"
"$AB" search '"from a subdirectory"'
echo "--- pending ---"
"$AB" pending
echo "--- metrics (excerpt) ---"
"$AB" metrics | head -30
echo "--- backup ---"
"$AB" backup now | head -12
echo "--- info (first lines) ---"
"$AB" info | head -8
echo "--- wrong token ---"
AGENT_BOARD_TOKEN=ab_bogus "$AB" recent || true
echo "--- server log tail ---"
tail -3 "$T/server.log"
echo E2E_OK
