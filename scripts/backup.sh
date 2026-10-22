#!/usr/bin/env bash
# Backs up the service working tree to /opt/shared/backup/<repo-name> with rsync.
# Mirrors the source (deletions propagate); excludes build output, vendored deps and
# the `common` symlink (it points at the shared checkout in /opt/shared/common).
#
# Usage: scripts/backup.sh [-n]   (-n = dry run)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
DEST_DIR="${BACKUP_ROOT:-/opt/shared/backup}/$(basename "$SRC_DIR")"

if ! command -v rsync >/dev/null; then
  echo "error: rsync not found — install it (e.g. sudo apt install rsync)" >&2
  exit 1
fi

opts=(-a --delete --human-readable --stats
  --exclude='/bin/'
  --exclude='/reports/'
  --exclude='/vendor/'
  --exclude='/common'
)
[[ "${1:-}" == "-n" ]] && opts+=(--dry-run --itemize-changes)

mkdir -p "$DEST_DIR"
rsync "${opts[@]}" "$SRC_DIR/" "$DEST_DIR/"
echo "backup complete: $SRC_DIR -> $DEST_DIR"
