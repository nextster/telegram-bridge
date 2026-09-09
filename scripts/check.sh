#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

command -v ffmpeg >/dev/null
command -v ffprobe >/dev/null
go test ./...
go vet ./...
python3 -m unittest discover -s scripts/tests -p 'test_*.py'
bash -n scripts/*.sh
scripts/verify-repo-boundaries.sh

if [[ "${SKIP_CODEX_PLUGIN_VALIDATE:-0}" != "1" ]]; then
  scripts/codex-plugin.sh validate
fi

if command -v fly >/dev/null 2>&1; then
  fly config validate --config fly.toml
  fly config validate --config fly.fast.toml
fi

git diff --check
