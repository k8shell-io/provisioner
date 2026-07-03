#!/usr/bin/env bash
# Attaches delve to the running api-server process for remote debugging.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_NAME="$(basename -s .git "$(git -C "$SCRIPT_DIR" config --get remote.origin.url)")"

BIN_NAME="$REPO_NAME"
DLV="/go/bin/dlv"
LISTEN_ADDR="${DLV_LISTEN:-127.0.0.1:2345}"

mapfile -t CHILD_PIDS < <(pgrep -x "$BIN_NAME")
if [[ ${#CHILD_PIDS[@]} -eq 0 ]]; then
  echo "error: no running ${BIN_NAME} process found" >&2
  exit 1
elif [[ ${#CHILD_PIDS[@]} -gt 1 ]]; then
  echo "error: multiple ${BIN_NAME} processes found (${CHILD_PIDS[*]}) — expected exactly one" >&2
  exit 1
fi
CHILD_PID="${CHILD_PIDS[0]}"

echo "attaching dlv to pid ${CHILD_PID}, listening on ${LISTEN_ADDR}"
exec sudo "$DLV" attach "$CHILD_PID" \
  --listen="$LISTEN_ADDR" \
  --headless=true \
  --api-version=2 \
  --accept-multiclient \
  --log \
  --only-same-user=false
