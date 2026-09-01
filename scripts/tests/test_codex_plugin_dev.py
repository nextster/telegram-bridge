from __future__ import annotations

import json
import os
import pathlib
import subprocess
import tempfile
import textwrap
import tomllib
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "codex_plugin_dev.py"


FAKE_CODEX = r'''#!/usr/bin/env python3
import json, os, pathlib, re, sys, tomllib

home = pathlib.Path(os.environ["CODEX_HOME"])
config = home / "config.toml"
args = sys.argv[1:]

if args[:2] == ["mcp", "add"]:
    name = args[2]
    marker = args.index("--")
    command = args[marker + 1]
    text = config.read_text() if config.exists() else ""
    text += f'\n[mcp_servers.{name}]\ncommand = {json.dumps(command)}\n'
    config.parent.mkdir(parents=True, exist_ok=True)
    config.write_text(text)
    print(f"Added global MCP server '{name}'.")
elif args[:2] == ["mcp", "remove"]:
    name = args[2]
    text = config.read_text() if config.exists() else ""
    pattern = rf'(?ms)^\[mcp_servers\.{re.escape(name)}\]\n.*?(?=^\[|\Z)'
    config.write_text(re.sub(pattern, "", text))
elif args[:2] == ["mcp", "get"]:
    data = tomllib.loads(config.read_text()) if config.exists() else {}
    server = data.get("mcp_servers", {}).get(args[2])
    if server and "command" in server:
        transport = {"type": "stdio", "command": server["command"], "args": server.get("args", []), "cwd": server.get("cwd")}
    else:
        transport = {"type": "streamable_http", "url": "https://telegram-bridge.fly.dev/mcp", "bearer_token_env_var": "TELEGRAM_BRIDGE_MCP_TOKEN"}
    print(json.dumps({"name": args[2], "enabled": True, "transport": transport}))
elif args[:3] == ["plugin", "list", "--json"]:
    version = os.environ.get("FAKE_PLUGIN_VERSION", "0.1.0+codex.test")
    source = os.environ["TELEGRAM_BRIDGE_REPO_ROOT"] + "/plugins/telegram-bridge"
    print(json.dumps({"installed": [{"pluginId": "telegram-bridge@telegram-bridge-repo", "version": version, "installed": True, "enabled": True, "source": {"source": "local", "path": source}}], "available": []}))
else:
    print("unsupported fake codex args: " + repr(args), file=sys.stderr)
    raise SystemExit(2)
'''


FAKE_LAUNCHCTL = "#!/bin/sh\nexit 1\n"


class DevPluginTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.home = pathlib.Path(self.temp.name)
        self.codex_home = self.home / ".codex"
        self.codex_home.mkdir()
        self.fake_codex = self.home / "codex"
        self.fake_codex.write_text(FAKE_CODEX)
        self.fake_codex.chmod(0o755)
        self.fake_launchctl = self.home / "launchctl"
        self.fake_launchctl.write_text(FAKE_LAUNCHCTL)
        self.fake_launchctl.chmod(0o755)
        version = json.loads((ROOT / "plugins/telegram-bridge/.codex-plugin/plugin.json").read_text())["version"]
        self.env = {
            **os.environ,
            "HOME": str(self.home),
            "CODEX_HOME": str(self.codex_home),
            "CODEX_BIN": str(self.fake_codex),
            "LAUNCHCTL_BIN": str(self.fake_launchctl),
            "TELEGRAM_BRIDGE_REPO_ROOT": str(ROOT),
            "FAKE_PLUGIN_VERSION": version,
        }

    def tearDown(self) -> None:
        self.temp.cleanup()

    def run_dev(self, command: str, *, check: bool = True) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            ["python3", str(SCRIPT), command],
            env=self.env,
            text=True,
            capture_output=True,
            check=check,
        )

    def config(self) -> dict:
        path = self.codex_home / "config.toml"
        return tomllib.loads(path.read_text()) if path.exists() else {}

    def test_link_status_unlink_are_idempotent_and_preserve_config(self) -> None:
        config = self.codex_home / "config.toml"
        config.write_text('[unrelated]\nvalue = "keep"\n')
        self.run_dev("link")
        self.run_dev("link")
        self.assertEqual(self.config()["unrelated"]["value"], "keep")
        command = self.config()["mcp_servers"]["telegram-bridge"]["command"]
        self.assertEqual(command, str(self.codex_home / "telegram-bridge-dev/mcp-bootstrap"))
        self.assertEqual((self.codex_home / "telegram-bridge-dev/source").read_text().strip(), str(ROOT))
        status = self.run_dev("status")
        self.assertIn("mode: development", status.stdout)
        self.assertIn(f"MCP cwd/source: {ROOT}", status.stdout)
        self.assertIn("sync: ok", status.stdout)
        self.run_dev("unlink")
        self.run_dev("unlink")
        self.assertEqual(self.config()["unrelated"]["value"], "keep")
        self.assertNotIn("telegram-bridge", self.config().get("mcp_servers", {}))
        status = self.run_dev("status")
        self.assertIn("mode: production", status.stdout)
        self.assertIn("MCP transport: streamable_http", status.stdout)

    def test_status_reports_stale_installed_version(self) -> None:
        self.env["FAKE_PLUGIN_VERSION"] = "0.1.0+codex.stale"
        result = self.run_dev("status", check=False)
        self.assertEqual(result.returncode, 1)
        self.assertIn("warning: plugin version mismatch", result.stdout)

    def test_status_reports_missing_or_moved_checkout(self) -> None:
        self.run_dev("link")
        pointer = self.codex_home / "telegram-bridge-dev/source"
        pointer.write_text(str(self.home / "moved-checkout") + "\n")
        result = self.run_dev("status", check=False)
        self.assertEqual(result.returncode, 1)
        self.assertIn("warning: linked checkout is missing or moved", result.stdout)
        bootstrap = self.codex_home / "telegram-bridge-dev/mcp-bootstrap"
        failed = subprocess.run([str(bootstrap)], env=self.env, text=True, capture_output=True)
        self.assertEqual(failed.returncode, 1)
        self.assertIn("checkout is missing or moved", failed.stderr)
        self.run_dev("unlink")

    def test_custom_override_is_never_replaced_or_removed(self) -> None:
        original = textwrap.dedent(
            '''
            [unrelated]
            value = "keep"

            [mcp_servers.telegram-bridge]
            command = "/custom/server"
            args = ["--safe"]
            '''
        )
        config = self.codex_home / "config.toml"
        config.write_text(original)
        result = self.run_dev("link", check=False)
        self.assertEqual(result.returncode, 1)
        self.assertIn("custom override", result.stderr)
        self.assertEqual(config.read_text(), original)
        self.run_dev("unlink")
        self.assertEqual(config.read_text(), original)


if __name__ == "__main__":
    unittest.main()
