#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

python3 - "${ROOT}" <<'PY'
import json
import pathlib
import re
import sys

root = pathlib.Path(sys.argv[1])
marketplace = json.loads((root / ".agents/plugins/marketplace.json").read_text())
plugin_root = root / "plugins/telegram-bridge"
plugin = json.loads((plugin_root / ".codex-plugin/plugin.json").read_text())
mcp = json.loads((plugin_root / ".mcp.json").read_text())
skill = (plugin_root / "skills/telegram-bridge/SKILL.md").read_text()
agent = (plugin_root / "skills/telegram-bridge/agents/openai.yaml").read_text()
env_example = (root / ".env.example").read_text()

assert marketplace["name"] == "nextster"
entries = [entry for entry in marketplace["plugins"] if entry.get("name") == "telegram-bridge"]
assert len(entries) == 1
assert entries[0]["source"] == {"source": "local", "path": "./plugins/telegram-bridge"}
assert entries[0]["policy"] == {"installation": "AVAILABLE", "authentication": "ON_INSTALL"}
assert plugin_root.name == plugin["name"] == "telegram-bridge"
assert re.fullmatch(r"0\.1\.0\+codex\.[A-Za-z0-9._-]+", plugin["version"])
assert plugin["repository"] == "https://github.com/nextster/telegram-bridge"
assert plugin["mcpServers"] == "./.mcp.json"
assert mcp == {
    "mcpServers": {
        "telegram-bridge": {
            "type": "http",
            "url": "https://telegram-bridge.fly.dev/mcp",
            "bearer_token_env_var": "TELEGRAM_BRIDGE_MCP_TOKEN",
        }
    }
}

assert skill.startswith("---\nname: telegram-bridge\ndescription:")
assert 'value: "telegram-bridge"' in agent
assert 'url: "https://telegram-bridge.fly.dev/mcp"' in agent
for name in (
    "TELEGRAM_BOT_TOKEN",
    "TELEGRAM_API_ID",
    "TELEGRAM_API_HASH",
    "TELEGRAM_BRIDGE_SESSION_KEY",
    "TELEGRAM_LOGIN_CLIENT_SECRET",
    "TELEGRAM_BRIDGE_MCP_TOKEN",
    "TELEGRAM_BRIDGE_NOTIFICATION_TOKEN",
    "TELEGRAM_BRIDGE_NOTIFICATION_CHAT_IDS",
    "OPENROUTER_API_KEY",
):
    assert re.search(rf"^{name}=$", env_example, re.MULTILINE), f"{name} must stay empty in .env.example"

for path in plugin_root.rglob("*"):
    assert not path.is_symlink(), f"plugin source must not contain symlinks: {path}"

print("repository plugin boundaries are valid")
PY
