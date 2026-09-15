#!/usr/bin/env python3
from __future__ import annotations

import json
import os
import pathlib
import shutil
import sys
import tempfile


PLUGIN_NAME = "telegram-bridge"
MANIFEST = pathlib.Path(".agents") / "plugins" / "marketplace.json"


def main() -> None:
    root = pathlib.Path(__file__).resolve().parent.parent
    marketplace_root = pathlib.Path(
        os.environ.get("NEXTSTER_MARKETPLACE_DIR", pathlib.Path.home() / ".agent-plugins" / "nextster")
    ).expanduser().resolve()
    if sys.argv[1:2] == ["--migrate-from"] and len(sys.argv) == 3:
        migrate_legacy(pathlib.Path(sys.argv[2]).expanduser(), marketplace_root)
    elif len(sys.argv) != 1:
        raise RuntimeError("usage: sync-nextster-marketplace.py [--migrate-from LEGACY_ROOT]")
    source_manifest = json.loads((root / ".agents" / "plugins" / "marketplace.json").read_text())
    entries = [entry for entry in source_manifest.get("plugins", []) if entry.get("name") == PLUGIN_NAME]
    if source_manifest.get("name") != "nextster" or len(entries) != 1:
        raise RuntimeError("invalid Nextster marketplace source")

    plugins_dir = marketplace_root / "plugins"
    plugins_dir.mkdir(parents=True, mode=0o700, exist_ok=True)
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

    manifest_path = marketplace_root / MANIFEST
    marketplace = read_manifest(manifest_path)
    marketplace["interface"] = {**marketplace.get("interface", {}), "displayName": "Nextster"}
    marketplace["plugins"] = [
        *[entry for entry in marketplace["plugins"] if entry.get("name") != PLUGIN_NAME],
        entries[0],
    ]
    write_manifest(manifest_path, marketplace)
    print(marketplace_root)


# Earlier installers kept the shared marketplace under CODEX_HOME. The caller
# detaches Codex from it first: Codex fails every plugin command once a
# registered root loses its manifest.
def migrate_legacy(legacy_root: pathlib.Path, marketplace_root: pathlib.Path) -> None:
    if legacy_root.is_symlink() or not legacy_root.is_dir():
        return
    if os.path.realpath(legacy_root) == os.path.realpath(marketplace_root):
        return
    if not marketplace_root.exists():
        marketplace_root.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
        shutil.move(legacy_root, marketplace_root)
    else:
        # A legacy root that reappears after migration was rewritten by a bridge
        # installer that still targets it, so its copies of other plugins are newer.
        legacy = read_manifest(legacy_root / MANIFEST)
        manifest_path = marketplace_root / MANIFEST
        marketplace = read_manifest(manifest_path)
        for entry in legacy["plugins"]:
            name = entry.get("name")
            if name == PLUGIN_NAME:
                continue
            if not isinstance(name, str) or name in ("", ".", "..") or pathlib.Path(name).name != name:
                raise RuntimeError(f"invalid plugin name {name!r} in {legacy_root / MANIFEST}")
            source = legacy_root / "plugins" / name
            destination = marketplace_root / "plugins" / name
            if source.exists():
                if destination.exists():
                    shutil.rmtree(destination)
                destination.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
                shutil.move(source, destination)
            marketplace["plugins"] = [*[item for item in marketplace["plugins"] if item.get("name") != name], entry]
        write_manifest(manifest_path, marketplace)
        shutil.rmtree(legacy_root)
    try:
        legacy_root.parent.rmdir()
    except OSError:
        pass


def read_manifest(path: pathlib.Path) -> dict:
    marketplace = {
        "name": "nextster",
        "interface": {"displayName": "Nextster"},
        "plugins": [],
    }
    if path.exists():
        marketplace = json.loads(path.read_text())
    if marketplace.get("name") != "nextster" or not isinstance(marketplace.get("plugins"), list):
        raise RuntimeError(f"invalid shared marketplace at {path}")
    return marketplace


def write_manifest(path: pathlib.Path, marketplace: dict) -> None:
    path.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
    temporary_manifest = path.with_name(f".{path.name}.{os.getpid()}.tmp")
    temporary_manifest.write_text(json.dumps(marketplace, indent=2) + "\n")
    temporary_manifest.chmod(0o600)
    temporary_manifest.replace(path)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(str(error), file=sys.stderr)
        raise SystemExit(1)
