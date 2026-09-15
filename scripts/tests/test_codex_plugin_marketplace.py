from __future__ import annotations

import json
import os
import pathlib
import subprocess
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "codex-plugin.sh"


# Mirrors the codex-cli behavior the installer relies on: listings fail once a
# registered root has no manifest, and plugin enabled state is kept by
# marketplace name when a marketplace is removed and added again.
FAKE_CODEX = r'''#!/usr/bin/env python3
import json, os, pathlib, sys

state_path = pathlib.Path(os.environ["FAKE_CODEX_STATE"])
state = json.loads(state_path.read_text())
args = [arg for arg in sys.argv[1:] if arg != "--json"]
legacy = pathlib.Path(os.environ["HOME"]) / ".codex" / "marketplaces" / "nextster"
with open(os.environ["FAKE_CODEX_LOG"], "a") as log:
    log.write(json.dumps({"args": args, "legacy_exists": legacy.exists()}) + "\n")

def manifest(root):
    path = pathlib.Path(root) / ".agents" / "plugins" / "marketplace.json"
    return json.loads(path.read_text()) if path.is_file() else None

def fail(message):
    print(message, file=sys.stderr)
    raise SystemExit(1)

def check_marketplaces():
    broken = [f"- `{name}` at {root}: marketplace root does not contain a supported manifest"
              for name, root in state["marketplaces"].items() if manifest(root) is None]
    if broken:
        fail("Error: failed to load marketplace(s):\n" + "\n".join(broken))

def save():
    state_path.write_text(json.dumps(state))

if args[:3] == ["plugin", "marketplace", "list"]:
    check_marketplaces()
    print(json.dumps({"marketplaces": [{"name": name, "root": root} for name, root in state["marketplaces"].items()]}))
elif args[:3] == ["plugin", "marketplace", "add"]:
    root = os.path.realpath(args[3])
    data = manifest(root)
    if data is None:
        fail(f"Error: {root} does not contain a supported manifest")
    current = state["marketplaces"].get(data["name"])
    if current and current != root:
        fail(f"Error: marketplace `{data['name']}` is already added from {current}")
    state["marketplaces"][data["name"]] = root
    save()
    print(json.dumps({"marketplaceName": data["name"], "installedRoot": root}))
elif args[:3] == ["plugin", "marketplace", "remove"]:
    if state["marketplaces"].pop(args[3], None) is None:
        fail(f"Error: marketplace `{args[3]}` is not configured or installed")
    save()
elif args[:2] == ["plugin", "list"]:
    check_marketplaces()
    only = args[args.index("--marketplace") + 1] if "--marketplace" in args else None
    installed, available = [], []
    for name, root in state["marketplaces"].items():
        if only and name != only:
            continue
        for entry in manifest(root)["plugins"]:
            plugin_id = f"{entry['name']}@{name}"
            item = {"pluginId": plugin_id, "source": {"source": "local", "path": str(pathlib.Path(root) / "plugins" / entry["name"])}}
            if plugin_id in state["plugins"]:
                installed.append({**item, "installed": True, "enabled": state["plugins"][plugin_id]})
            elif "--available" in args:
                available.append(item)
    print(json.dumps({"installed": installed, "available": available}))
elif args[:2] == ["plugin", "add"]:
    check_marketplaces()
    name, marketplace = args[2].split("@")
    root = state["marketplaces"].get(marketplace)
    if not root or not any(entry["name"] == name for entry in manifest(root)["plugins"]):
        fail(f"Error: plugin {args[2]} is not available")
    state["plugins"][args[2]] = True
    save()
elif args[:2] == ["plugin", "remove"]:
    if state["plugins"].pop(args[2], None) is None:
        fail(f"Error: plugin {args[2]} is not installed")
    save()
else:
    fail("unsupported fake codex args: " + repr(args))
'''


class CodexPluginMarketplaceTest(unittest.TestCase):
    def setUp(self) -> None:
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.base = pathlib.Path(os.path.realpath(temporary.name))
        self.home = self.base / "home"
        self.root = self.home / ".agent-plugins" / "nextster"
        self.legacy = self.home / ".codex" / "marketplaces" / "nextster"
        self.home.mkdir()
        bin_dir = self.base / "bin"
        bin_dir.mkdir()
        self.codex = bin_dir / "codex"
        self.codex.write_text(FAKE_CODEX)
        self.codex.chmod(0o755)
        creator = self.base / "creator" / "scripts"
        creator.mkdir(parents=True)
        (creator / "read_marketplace_name.py").write_text("import json, sys\nprint(json.load(open(sys.argv[2]))['name'])\n")
        (creator / "validate_plugin.py").write_text("")
        (creator / "update_plugin_cachebuster.py").write_text("")
        (creator / "quick_validate.py").write_text("")
        self.state_path = self.base / "codex-state.json"
        self.log_path = self.base / "codex-log.jsonl"
        self.log_path.write_text("")
        self.env = {key: value for key, value in os.environ.items() if key not in ("CODEX_HOME", "NEXTSTER_MARKETPLACE_DIR")}
        self.env.update({
            "HOME": str(self.home),
            "CODEX_BIN": str(self.codex),
            "PLUGIN_CREATOR_ROOT": str(creator.parent),
            "SKILL_CREATOR_ROOT": str(creator.parent),
            "FAKE_CODEX_STATE": str(self.state_path),
            "FAKE_CODEX_LOG": str(self.log_path),
        })

    def write_marketplace(self, root: pathlib.Path, names: list[str]) -> None:
        for name in names:
            plugin = root / "plugins" / name
            plugin.mkdir(parents=True, exist_ok=True)
            (plugin / "marker.txt").write_text(f"{name} from {root}")
        manifest = root / ".agents" / "plugins" / "marketplace.json"
        manifest.parent.mkdir(parents=True, exist_ok=True)
        manifest.write_text(json.dumps({
            "name": "nextster",
            "plugins": [{"name": name, "source": {"source": "local", "path": f"./plugins/{name}"}} for name in names],
        }))

    def register(self, root: pathlib.Path, plugins: dict[str, bool]) -> None:
        self.state_path.write_text(json.dumps({"marketplaces": {"nextster": str(root)}, "plugins": plugins}))

    def install(self, **env: str) -> subprocess.CompletedProcess[str]:
        return subprocess.run(["bash", str(SCRIPT), "install"], capture_output=True, text=True, env={**self.env, **env})

    def state(self) -> dict:
        return json.loads(self.state_path.read_text())

    def calls(self) -> list[dict]:
        return [json.loads(line) for line in self.log_path.read_text().splitlines()]

    def plugin_names(self, root: pathlib.Path) -> list[str]:
        manifest = json.loads((root / ".agents" / "plugins" / "marketplace.json").read_text())
        return [entry["name"] for entry in manifest["plugins"]]

    def assert_installed(self, result: subprocess.CompletedProcess[str]) -> None:
        self.assertEqual(result.returncode, 0, result.stderr)
        state = self.state()
        self.assertEqual(state["marketplaces"], {"nextster": str(self.root)})
        self.assertTrue(state["plugins"]["figma-bridge@nextster"])
        self.assertTrue(state["plugins"]["telegram-bridge@nextster"])
        self.assertFalse(self.legacy.exists())
        self.assertFalse(self.legacy.parent.exists())
        self.assertTrue((self.root / "plugins" / "telegram-bridge" / ".mcp.json").is_file())

    def test_moves_legacy_marketplace_after_detaching_codex(self) -> None:
        self.write_marketplace(self.legacy, ["figma-bridge", "telegram-bridge"])
        self.register(self.legacy, {"figma-bridge@nextster": True, "telegram-bridge@nextster": True})

        result = self.install()

        self.assert_installed(result)
        removals = [call for call in self.calls() if call["args"] == ["plugin", "marketplace", "remove", "nextster"]]
        self.assertEqual(removals, [{"args": ["plugin", "marketplace", "remove", "nextster"], "legacy_exists": True}])
        self.assertEqual(self.plugin_names(self.root), ["figma-bridge", "telegram-bridge"])
        self.assertEqual((self.root / "plugins" / "figma-bridge" / "marker.txt").read_text(), f"figma-bridge from {self.legacy}")

    def test_merges_legacy_marketplace_into_existing_root(self) -> None:
        self.write_marketplace(self.root, ["chromium-bridge"])
        claude_manifest = self.root / ".claude-plugin" / "marketplace.json"
        claude_manifest.parent.mkdir()
        claude_manifest.write_text('{"name": "nextster", "plugins": [{"name": "chromium-bridge"}]}\n')
        (self.root / "README.md").write_text("written by chromium-bridge\n")
        self.write_marketplace(self.legacy, ["figma-bridge", "telegram-bridge"])
        self.register(self.legacy, {"chromium-bridge@nextster": True, "figma-bridge@nextster": True})

        result = self.install()

        self.assert_installed(result)
        self.assertTrue(self.state()["plugins"]["chromium-bridge@nextster"])
        self.assertEqual(self.plugin_names(self.root), ["chromium-bridge", "figma-bridge", "telegram-bridge"])
        self.assertEqual((self.root / "plugins" / "chromium-bridge" / "marker.txt").read_text(), f"chromium-bridge from {self.root}")
        self.assertEqual((self.root / "plugins" / "figma-bridge" / "marker.txt").read_text(), f"figma-bridge from {self.legacy}")
        self.assertEqual(claude_manifest.read_text(), '{"name": "nextster", "plugins": [{"name": "chromium-bridge"}]}\n')
        self.assertEqual((self.root / "README.md").read_text(), "written by chromium-bridge\n")

    def test_detaches_registration_whose_root_has_no_manifest(self) -> None:
        # An interrupted migration moved the directory but left Codex on the old root.
        self.write_marketplace(self.root, ["figma-bridge"])
        self.register(self.legacy, {"figma-bridge@nextster": True})

        result = self.install()

        self.assert_installed(result)
        self.assertEqual(self.plugin_names(self.root), ["figma-bridge", "telegram-bridge"])

    def test_compares_marketplace_roots_canonically(self) -> None:
        real_root = self.base / "real-marketplace"
        self.write_marketplace(real_root, ["figma-bridge"])
        link = self.base / "linked-marketplace"
        link.symlink_to(real_root)
        self.register(real_root, {"figma-bridge@nextster": True})

        result = self.install(NEXTSTER_MARKETPLACE_DIR=str(link))

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn(["plugin", "marketplace", "remove", "nextster"], [call["args"] for call in self.calls()])
        self.assertEqual(self.state()["marketplaces"], {"nextster": str(real_root)})
        self.assertEqual(self.plugin_names(real_root), ["figma-bridge", "telegram-bridge"])
        self.assertFalse(self.root.exists())

    def test_refuses_registration_at_another_root_with_manifest(self) -> None:
        other = self.base / "other-marketplace"
        self.write_marketplace(other, ["figma-bridge"])
        self.write_marketplace(self.legacy, ["figma-bridge"])
        self.register(other, {"figma-bridge@nextster": True})

        result = self.install()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn(f"already points to {other}", result.stderr)
        self.assertNotIn(["plugin", "marketplace", "remove", "nextster"], [call["args"] for call in self.calls()])
        self.assertEqual(self.state()["marketplaces"], {"nextster": str(other)})
        self.assertTrue((self.legacy / ".agents" / "plugins" / "marketplace.json").is_file())
        self.assertFalse(self.root.exists())

    def test_restores_registration_when_migration_fails(self) -> None:
        self.write_marketplace(self.legacy, ["figma-bridge", "telegram-bridge"])
        broken = self.root / ".agents" / "plugins" / "marketplace.json"
        broken.parent.mkdir(parents=True)
        broken.write_text('{"name": "someone-else", "plugins": []}')
        self.register(self.legacy, {"figma-bridge@nextster": True})

        result = self.install()

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("invalid shared marketplace", result.stderr)
        self.assertEqual(self.state()["marketplaces"], {"nextster": str(self.legacy)})
        self.assertTrue(self.state()["plugins"]["figma-bridge@nextster"])
        self.assertEqual(self.plugin_names(self.legacy), ["figma-bridge", "telegram-bridge"])


if __name__ == "__main__":
    unittest.main()
