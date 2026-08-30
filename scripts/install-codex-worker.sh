#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LABEL="dev.nextster.telegram-bridge.codex-worker"
APP_DIR="${HOME}/Library/Application Support/telegram-bridge"
BIN="${APP_DIR}/telegram-bridge"
PLIST="${HOME}/Library/LaunchAgents/${LABEL}.plist"
LOG_DIR="${HOME}/Library/Logs"
CODEX_BIN="$(command -v codex || true)"

usage() {
  echo "usage: $0 slug=/absolute/project/path [slug=/path ...]" >&2
  exit 2
}

[[ "$#" -gt 0 ]] || usage
[[ -n "${CODEX_BIN}" && -x "${CODEX_BIN}" ]] || {
  echo "codex CLI was not found in the installer PATH" >&2
  exit 1
}
for mapping in "$@"; do
  slug="${mapping%%=*}"
  path="${mapping#*=}"
  [[ -n "${slug}" && -n "${path}" && "${path}" = /* && -d "${path}" ]] || usage
done

mkdir -p "${APP_DIR}" "$(dirname "${PLIST}")" "${LOG_DIR}"
go build -o "${BIN}" ./cmd/telegram-bridge

arguments=("${BIN}" worker --codex-bin "${CODEX_BIN}")
for mapping in "$@"; do
  arguments+=(--project "${mapping}")
done

{
  cat <<PLIST_HEAD
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>${LABEL}</string>
  <key>ProgramArguments</key>
  <array>
PLIST_HEAD
  for argument in "${arguments[@]}"; do
    escaped="$(printf '%s' "${argument}" | sed 's/&/\&amp;/g; s/</\&lt;/g; s/>/\&gt;/g')"
    printf '    <string>%s</string>\n' "${escaped}"
  done
  cat <<PLIST_TAIL
  </array>
  <key>WorkingDirectory</key><string>${ROOT}</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>${LOG_DIR}/telegram-bridge-worker.log</string>
  <key>StandardErrorPath</key><string>${LOG_DIR}/telegram-bridge-worker.log</string>
</dict>
</plist>
PLIST_TAIL
} > "${PLIST}"

plutil -lint "${PLIST}"
launchctl bootout "gui/${UID}/${LABEL}" 2>/dev/null || true
launchctl bootstrap "gui/${UID}" "${PLIST}"
launchctl kickstart -k "gui/${UID}/${LABEL}"
echo "Installed ${LABEL}; log: ${LOG_DIR}/telegram-bridge-worker.log"
