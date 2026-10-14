#!/usr/bin/env bash

runtime_layout_path() {
  local store_root definition_hash
  store_root="${RUNTIME_STORE_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/telegram-bridge/runtimes}"
  definition_hash="$(shasum -a 256 "$ROOT/Dockerfile.runtime" | awk '{print $1}')"
  printf '%s/%s-%s\n' "$store_root" "${PLATFORM//\//-}" "$definition_hash"
}

runtime_image_context() {
  local layout
  layout="$(runtime_layout_path)"
  if [[ ! -f "$layout/index.json" ]]; then
    printf 'Prepared runtime for %s is missing. Run PLATFORM=%s scripts/build-runtime.sh on a compatible builder first.\n' "$PLATFORM" "$PLATFORM" >&2
    return 1
  fi
  python3 - "$layout" <<'PY'
import json
from pathlib import Path
import sys

layout = Path(sys.argv[1]).resolve()
index = json.loads((layout / "index.json").read_text())
digest = index["manifests"][0]["digest"]
print(f"oci-layout://{layout}@{digest}")
PY
}
