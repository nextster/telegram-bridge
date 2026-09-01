#!/usr/bin/env bash
set -euo pipefail

CODEX_HOME="${CODEX_HOME:-${HOME}/.codex}"
POINTER="${CODEX_HOME}/telegram-bridge-dev/source"

if [[ ! -f "${POINTER}" ]]; then
  echo "telegram-bridge dev source pointer is missing; run scripts/codex-plugin.sh dev:link" >&2
  exit 1
fi

SOURCE="$(<"${POINTER}")"
if [[ "${SOURCE}" != /* || ! -d "${SOURCE}" ]]; then
  echo "telegram-bridge dev checkout is missing or moved: ${SOURCE}" >&2
  exit 1
fi
if [[ ! -x "${SOURCE}/scripts/telegram-bridge-mcp-dev.sh" ]]; then
  echo "telegram-bridge dev adapter is missing: ${SOURCE}/scripts/telegram-bridge-mcp-dev.sh" >&2
  exit 1
fi
if [[ "$(git -C "${SOURCE}" rev-parse --show-toplevel 2>/dev/null || true)" != "${SOURCE}" ]]; then
  echo "telegram-bridge dev pointer does not name a Git root: ${SOURCE}" >&2
  exit 1
fi

exec "${SOURCE}/scripts/telegram-bridge-mcp-dev.sh"
