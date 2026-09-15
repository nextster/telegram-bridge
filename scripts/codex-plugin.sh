#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PLUGIN_NAME="telegram-bridge"
PLUGIN_ROOT="${ROOT}/plugins/${PLUGIN_NAME}"
MARKETPLACE_FILE="${ROOT}/.agents/plugins/marketplace.json"
CODEX_HOME="${CODEX_HOME:-${HOME}/.codex}"
SHARED_MARKETPLACE_ROOT="${NEXTSTER_MARKETPLACE_DIR:-${CODEX_HOME}/marketplaces/nextster}"
CODEX_BIN="${CODEX_BIN:-codex}"
PLUGIN_CREATOR_ROOT="${PLUGIN_CREATOR_ROOT:-${CODEX_HOME}/skills/.system/plugin-creator}"
SKILL_CREATOR_ROOT="${SKILL_CREATOR_ROOT:-${CODEX_HOME}/skills/.system/skill-creator}"

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf 'required command not found: %s\n' "$1" >&2
    exit 1
  fi
}

require_file() {
  if [[ ! -f "$1" ]]; then
    printf 'required file not found: %s\n' "$1" >&2
    exit 1
  fi
}

dev_command() {
  require_command python3
  python3 "${ROOT}/scripts/codex_plugin_dev.py" "$1"
}

prepare() {
  require_command "${CODEX_BIN}"
  require_command python3
  require_file "${MARKETPLACE_FILE}"
  require_file "${PLUGIN_CREATOR_ROOT}/scripts/read_marketplace_name.py"
  require_file "${PLUGIN_CREATOR_ROOT}/scripts/validate_plugin.py"
  require_file "${PLUGIN_CREATOR_ROOT}/scripts/update_plugin_cachebuster.py"
  require_file "${SKILL_CREATOR_ROOT}/scripts/quick_validate.py"
}

marketplace_name() {
  python3 "${PLUGIN_CREATOR_ROOT}/scripts/read_marketplace_name.py" \
    --marketplace-path "${MARKETPLACE_FILE}"
}

validate() {
  prepare
  python3 "${SKILL_CREATOR_ROOT}/scripts/quick_validate.py" \
    "${PLUGIN_ROOT}/skills/telegram-bridge"
  python3 "${PLUGIN_CREATOR_ROOT}/scripts/validate_plugin.py" "${PLUGIN_ROOT}"
}

registered_marketplace_root() {
  local name="$1"
  "${CODEX_BIN}" plugin marketplace list | awk -v name="${name}" '$1 == name { print $2; exit }'
}

plugin_is_installed() {
  local selector="$1"
  "${CODEX_BIN}" plugin list --json | python3 -c '
import json, sys
selector = sys.argv[1]
payload = json.load(sys.stdin)
raise SystemExit(0 if any(item.get("pluginId") == selector and item.get("installed") for item in payload.get("installed", [])) else 1)
' "${selector}"
}

verify_repo_plugin_available() {
  local name expected_path
  name="$(marketplace_name)"
  expected_path="$(cd "${SHARED_MARKETPLACE_ROOT}/plugins/${PLUGIN_NAME}" && pwd -P)"
  "${CODEX_BIN}" plugin list --marketplace "${name}" --available --json | python3 -c '
import json, os, sys
selector, expected_path = sys.argv[1:]
payload = json.load(sys.stdin)
items = payload.get("installed", []) + payload.get("available", [])
matches = [item for item in items if item.get("pluginId") == selector]
if len(matches) != 1 or os.path.realpath(matches[0].get("source", {}).get("path", "")) != os.path.realpath(expected_path):
    raise SystemExit(f"repo plugin is unavailable or resolves outside the repo: {matches!r}")
' "${PLUGIN_NAME}@${name}" "${expected_path}"
}

verify_repo_plugin_installed() {
  local name expected_path
  name="$(marketplace_name)"
  expected_path="$(cd "${SHARED_MARKETPLACE_ROOT}/plugins/${PLUGIN_NAME}" && pwd -P)"
  "${CODEX_BIN}" plugin list --marketplace "${name}" --json | python3 -c '
import json, os, sys
selector, expected_path = sys.argv[1:]
payload = json.load(sys.stdin)
matches = [item for item in payload.get("installed", []) if item.get("pluginId") == selector]
if len(matches) != 1:
    raise SystemExit(f"repo plugin is not installed exactly once: {matches!r}")
item = matches[0]
if not item.get("enabled") or os.path.realpath(item.get("source", {}).get("path", "")) != os.path.realpath(expected_path):
    raise SystemExit(f"repo plugin installation is not enabled or has the wrong source: {item!r}")
' "${PLUGIN_NAME}@${name}" "${expected_path}"
}

ensure_marketplace() {
  local name registered_root
  name="$(marketplace_name)"
  registered_root="$(registered_marketplace_root "${name}")"
  if [[ -n "${registered_root}" ]] && \
     [[ "$(cd "${registered_root}" && pwd -P)" == "$(cd "${ROOT}" && pwd -P)" ]]; then
    "${CODEX_BIN}" plugin remove "${PLUGIN_NAME}@telegram-bridge-repo" --json >/dev/null 2>&1 || true
    "${CODEX_BIN}" plugin marketplace remove telegram-bridge-repo --json >/dev/null 2>&1 || true
    "${CODEX_BIN}" plugin marketplace remove "${name}" --json >/dev/null 2>&1 || true
    registered_root=""
  fi
  if [[ -z "${registered_root}" ]]; then
    "${CODEX_BIN}" plugin marketplace add "${SHARED_MARKETPLACE_ROOT}"
    return
  fi
  if [[ "$(cd "${registered_root}" && pwd -P)" != "$(cd "${SHARED_MARKETPLACE_ROOT}" && pwd -P)" ]]; then
    printf 'marketplace %s already points to %s, expected %s\n' \
      "${name}" "${registered_root}" "${SHARED_MARKETPLACE_ROOT}" >&2
    exit 1
  fi
}

install_plugin() {
  local name
  validate
  python3 "${ROOT}/scripts/sync-nextster-marketplace.py" >/dev/null
  ensure_marketplace
  verify_repo_plugin_available
  name="$(marketplace_name)"
  "${CODEX_BIN}" plugin remove "${PLUGIN_NAME}@telegram-bridge-repo" --json >/dev/null 2>&1 || true
  "${CODEX_BIN}" plugin marketplace remove telegram-bridge-repo --json >/dev/null 2>&1 || true
  "${CODEX_BIN}" plugin add "${PLUGIN_NAME}@${name}"
  verify_repo_plugin_installed
}

usage() {
  cat <<'EOF'
Usage: scripts/codex-plugin.sh <validate|install|reload|dev:link|dev:status|dev:unlink>

  validate  Validate the tracked skill and plugin manifests.
  install   Register the repo marketplace if needed and install its current version.
  reload    Refresh the tracked cachebuster, validate, and reinstall.
  dev:link  Point future MCP processes at this checkout without reinstalling the plugin.
  dev:status Show the repo, installed plugin, effective MCP, and drift.
  dev:unlink Remove the dev override and return future tasks to the production plugin.
EOF
}

case "${1:-}" in
  validate)
    validate
    ;;
  install)
    install_plugin
    ;;
  reload)
    validate
    python3 "${PLUGIN_CREATOR_ROOT}/scripts/update_plugin_cachebuster.py" "${PLUGIN_ROOT}"
    install_plugin
    ;;
  dev:link)
    dev_command link
    ;;
  dev:status)
    dev_command status
    ;;
  dev:unlink)
    dev_command unlink
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac
