#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PLATFORM="${PLATFORM:-linux/amd64}"
source "$ROOT/scripts/runtime-cache.sh"

layout="$(runtime_layout_path)"
mkdir -p "$(dirname "$layout")"
staging="$(mktemp -d "$(dirname "$layout")/.runtime-build.XXXXXX")"
trap 'rm -rf "$staging"' EXIT
build_command=(docker buildx build)
if [[ -n "${RUNTIME_BUILDER:-}" ]]; then
  build_command+=(--builder "$RUNTIME_BUILDER")
fi
"${build_command[@]}" \
  --platform "$PLATFORM" \
  --file "$ROOT/Dockerfile.runtime" \
  --provenance=false \
  --output "type=oci,dest=$staging/runtime.tar" \
  "$ROOT"
mkdir "$staging/layout"
tar -xf "$staging/runtime.tar" -C "$staging/layout"
rm -rf "$layout"
mv "$staging/layout" "$layout"
printf 'runtime=%s\n' "$layout"
