#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PLUGIN_NAME="telegram-bridge"
PLUGIN_ROOT="${ROOT}/plugins/${PLUGIN_NAME}"
MARKETPLACE_FILE="${ROOT}/.agents/plugins/marketplace.json"
CODEX_HOME="${CODEX_HOME:-${HOME}/.codex}"
SHARED_MARKETPLACE_ROOT="${NEXTSTER_MARKETPLACE_DIR:-${HOME}/.agent-plugins/nextster}"
LEGACY_MARKETPLACE_ROOT="${CODEX_HOME}/marketplaces/nextster"
DETACHED_MARKETPLACE_ROOT=""
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

canonical_path() {
  python3 -c 'import os, sys; print(os.path.realpath(os.path.expanduser(sys.argv[1])))' "$1"
}

has_marketplace_manifest() {
  [[ -f "$1/.agents/plugins/marketplace.json" ]]
}

# Codex fails every listing once a registered root loses its manifest; its
# error names that marketplace and root.
registered_marketplace_root() {
  local name="$1" output
  if output="$("${CODEX_BIN}" plugin marketplace list --json 2>/dev/null)"; then
    python3 -c '
import json, sys
name = sys.argv[1]
roots = [item.get("root", "") for item in json.load(sys.stdin).get("marketplaces", []) if item.get("name") == name]
print(roots[0] if roots else "")
' "${name}" <<<"${output}"
    return
  fi
  output="$("${CODEX_BIN}" plugin marketplace list --json 2>&1 >/dev/null || true)"
  python3 -c '
import re, sys
name, output = sys.argv[1:]
pattern = re.compile(rf"^- `{re.escape(name)}` at (.+): marketplace root does not contain a supported manifest$", re.MULTILINE)
match = pattern.search(output)
if not match:
    raise SystemExit(output)
print(match.group(1))
' "${name}" "${output}"
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

# Detaches Codex from the legacy root, the repo root, or a root without a
# manifest before the legacy directory moves. Codex keeps plugin enabled state
# by marketplace name, so re-adding nextster at the new root keeps siblings.
detach_marketplace() {
  local name registered_root canonical_root
  name="$(marketplace_name)"
  registered_root="$(registered_marketplace_root "${name}")"
  [[ -n "${registered_root}" ]] || return 0
  canonical_root="$(canonical_path "${registered_root}")"
  [[ "${canonical_root}" != "$(canonical_path "${SHARED_MARKETPLACE_ROOT}")" ]] || return 0
  if [[ "${canonical_root}" != "$(canonical_path "${LEGACY_MARKETPLACE_ROOT}")" ]] && \
     [[ "${canonical_root}" != "$(canonical_path "${ROOT}")" ]] && \
     has_marketplace_manifest "${registered_root}"; then
    printf 'marketplace %s already points to %s, expected %s\n' \
      "${name}" "${registered_root}" "${SHARED_MARKETPLACE_ROOT}" >&2
    exit 1
  fi
  "${CODEX_BIN}" plugin marketplace remove "${name}" --json >/dev/null
  DETACHED_MARKETPLACE_ROOT="${registered_root}"
}

# Restores a registration detached by a run that fails before re-adding it.
restore_marketplace() {
  local status=$? candidate
  if [[ ${status} -ne 0 && -n "${DETACHED_MARKETPLACE_ROOT}" ]]; then
    for candidate in "${DETACHED_MARKETPLACE_ROOT}" "${SHARED_MARKETPLACE_ROOT}" "${LEGACY_MARKETPLACE_ROOT}"; do
      if has_marketplace_manifest "${candidate}"; then
        printf 'restoring marketplace registration at %s\n' "${candidate}" >&2
        "${CODEX_BIN}" plugin marketplace add "${candidate}" >/dev/null 2>&1 || true
        break
      fi
    done
  fi
}

ensure_marketplace() {
  local name registered_root
  name="$(marketplace_name)"
  registered_root="$(registered_marketplace_root "${name}")"
  if [[ -z "${registered_root}" ]]; then
    "${CODEX_BIN}" plugin marketplace add "${SHARED_MARKETPLACE_ROOT}"
    DETACHED_MARKETPLACE_ROOT=""
    return
  fi
  if [[ "$(canonical_path "${registered_root}")" != "$(canonical_path "${SHARED_MARKETPLACE_ROOT}")" ]]; then
    printf 'marketplace %s already points to %s, expected %s\n' \
      "${name}" "${registered_root}" "${SHARED_MARKETPLACE_ROOT}" >&2
    exit 1
  fi
}

install_plugin() {
  local name
  validate
  trap restore_marketplace EXIT
  detach_marketplace
  python3 "${ROOT}/scripts/sync-nextster-marketplace.py" --migrate-from "${LEGACY_MARKETPLACE_ROOT}" >/dev/null
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
