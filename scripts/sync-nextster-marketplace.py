#!/usr/bin/env python3
from __future__ import annotations

import json
import os
import pathlib
import shutil
import sys
import tempfile


PLUGIN_NAME = "telegram-bridge"


def main() -> None:
    root = pathlib.Path(__file__).resolve().parent.parent
    codex_home = pathlib.Path(os.environ.get("CODEX_HOME", pathlib.Path.home() / ".codex"))
    marketplace_root = pathlib.Path(
        os.environ.get("NEXTSTER_MARKETPLACE_DIR", codex_home / "marketplaces" / "nextster")
    ).expanduser().resolve()
    source_manifest = json.loads((root / ".agents" / "plugins" / "marketplace.json").read_text())
    entries = [entry for entry in source_manifest.get("plugins", []) if entry.get("name") == PLUGIN_NAME]
    if source_manifest.get("name") != "nextster" or len(entries) != 1:
        raise RuntimeError("invalid Nextster marketplace source")

    plugins_dir = marketplace_root / "plugins"
    manifest_dir = marketplace_root / ".agents" / "plugins"
    plugins_dir.mkdir(parents=True, mode=0o700, exist_ok=True)
    manifest_dir.mkdir(parents=True, mode=0o700, exist_ok=True)
    destination = plugins_dir / PLUGIN_NAME
    temporary = pathlib.Path(tempfile.mkdtemp(prefix=f".{PLUGIN_NAME}.tmp-", dir=plugins_dir))
    shutil.rmtree(temporary)
    try:
        shutil.copytree(root / "plugins" / PLUGIN_NAME, temporary)
        if destination.exists():
            shutil.rmtree(destination)
        temporary.replace(destination)
    finally:
        if temporary.exists():
            shutil.rmtree(temporary)

    manifest_path = manifest_dir / "marketplace.json"
    marketplace = {
        "name": "nextster",
        "interface": {"displayName": "Nextster"},
        "plugins": [],
    }
    if manifest_path.exists():
        marketplace = json.loads(manifest_path.read_text())
    if marketplace.get("name") != "nextster" or not isinstance(marketplace.get("plugins"), list):
        raise RuntimeError(f"invalid shared marketplace at {manifest_path}")
    marketplace["interface"] = {**marketplace.get("interface", {}), "displayName": "Nextster"}
    marketplace["plugins"] = [
        *[entry for entry in marketplace["plugins"] if entry.get("name") != PLUGIN_NAME],
        entries[0],
    ]
    temporary_manifest = manifest_path.with_name(f".{manifest_path.name}.{os.getpid()}.tmp")
    temporary_manifest.write_text(json.dumps(marketplace, indent=2) + "\n")
    temporary_manifest.chmod(0o600)
    temporary_manifest.replace(manifest_path)
    print(marketplace_root)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(str(error), file=sys.stderr)
        raise SystemExit(1)
