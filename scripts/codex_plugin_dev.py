#!/usr/bin/env python3
from __future__ import annotations

import json
import os
import pathlib
import plistlib
import subprocess
import sys
import tempfile
import tomllib

PLUGIN_NAME = "telegram-bridge"
PLUGIN_ID = "telegram-bridge@nextster"
WORKER_LABEL = "dev.nextster.telegram-bridge.codex-worker"


def repo_root() -> pathlib.Path:
    override = os.environ.get("TELEGRAM_BRIDGE_REPO_ROOT")
    return pathlib.Path(override or pathlib.Path(__file__).resolve().parent.parent).resolve()


def codex_home() -> pathlib.Path:
    return pathlib.Path(os.environ.get("CODEX_HOME", pathlib.Path.home() / ".codex")).expanduser()


def dev_home() -> pathlib.Path:
    return codex_home() / "telegram-bridge-dev"


def bootstrap_path() -> pathlib.Path:
    return dev_home() / "mcp-bootstrap"


def pointer_path() -> pathlib.Path:
    return dev_home() / "source"


def config_path() -> pathlib.Path:
    return codex_home() / "config.toml"


def codex_bin() -> str:
    return os.environ.get("CODEX_BIN", "codex")


def run(args: list[str], *, check: bool = True) -> subprocess.CompletedProcess[str]:
    return subprocess.run(args, check=check, text=True, capture_output=True)


def require_repo(root: pathlib.Path) -> None:
    required = [
        root / ".git",
        root / "go.mod",
        root / "plugins" / PLUGIN_NAME / ".codex-plugin" / "plugin.json",
        root / "scripts" / "telegram-bridge-mcp-bootstrap.sh",
        root / "scripts" / "telegram-bridge-mcp-dev.sh",
    ]
    missing = [str(path) for path in required if not path.exists()]
    if missing:
        raise RuntimeError("not a complete telegram-bridge checkout; missing: " + ", ".join(missing))
    result = run(["git", "-C", str(root), "rev-parse", "--show-toplevel"])
    if pathlib.Path(result.stdout.strip()).resolve() != root:
        raise RuntimeError(f"checkout is not its Git root: {root}")


def load_config() -> dict:
    path = config_path()
    if not path.exists():
        return {}
    with path.open("rb") as handle:
        return tomllib.load(handle)


def explicit_server() -> dict | None:
    value = load_config().get("mcp_servers", {}).get(PLUGIN_NAME)
    return value if isinstance(value, dict) else None


def is_our_server(server: dict | None) -> bool:
    if not server:
        return False
    command = server.get("command")
    if not isinstance(command, str):
        return False
    try:
        return pathlib.Path(command).expanduser().resolve() == bootstrap_path().resolve()
    except OSError:
        return False


def atomic_write(path: pathlib.Path, data: bytes, mode: int) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    temp_path = pathlib.Path(temporary)
    try:
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        temp_path.chmod(mode)
        temp_path.replace(path)
    finally:
        if temp_path.exists():
            temp_path.unlink()


def link() -> None:
    root = repo_root()
    require_repo(root)
    server = explicit_server()
    if server and not is_our_server(server):
        raise RuntimeError(
            "mcp_servers.telegram-bridge already contains a custom override; it was preserved"
        )
    bootstrap = root / "scripts" / "telegram-bridge-mcp-bootstrap.sh"
    atomic_write(bootstrap_path(), bootstrap.read_bytes(), 0o755)
    atomic_write(pointer_path(), (str(root) + "\n").encode(), 0o600)
    if not is_our_server(server):
        try:
            run([codex_bin(), "mcp", "add", PLUGIN_NAME, "--", str(bootstrap_path())])
        except subprocess.CalledProcessError:
            remove_dev_files()
            raise
    print(f"development MCP linked: {root}")
    print("new Codex tasks will load the checkout; existing tasks are unchanged")


def remove_dev_files() -> None:
    for path in (pointer_path(), bootstrap_path()):
        if path.exists() or path.is_symlink():
            path.unlink()
    try:
        dev_home().rmdir()
    except (FileNotFoundError, OSError):
        pass


def unlink() -> None:
    server = explicit_server()
    if is_our_server(server):
        run([codex_bin(), "mcp", "remove", PLUGIN_NAME])
    elif server:
        print("custom telegram-bridge MCP override preserved")
    remove_dev_files()
    print("development MCP unlinked; the installed versioned plugin is active for new tasks")


def plugin_details() -> dict | None:
    result = run([codex_bin(), "plugin", "list", "--json"], check=False)
    if result.returncode:
        return None
    try:
        payload = json.loads(result.stdout)
    except json.JSONDecodeError:
        return None
    for item in payload.get("installed", []):
        if item.get("pluginId") == PLUGIN_ID:
            return item
    return None


def mcp_details() -> dict | None:
    result = run([codex_bin(), "mcp", "get", PLUGIN_NAME, "--json"], check=False)
    if result.returncode:
        return None
    try:
        return json.loads(result.stdout)
    except json.JSONDecodeError:
        return None


def git_details(root: pathlib.Path) -> tuple[str, bool]:
    if not (root / ".git").exists():
        return "missing", False
    commit = run(["git", "-C", str(root), "rev-parse", "--short=12", "HEAD"], check=False)
    dirty = run(["git", "-C", str(root), "status", "--porcelain"], check=False)
    return commit.stdout.strip() or "unknown", bool(dirty.stdout.strip())


def repo_version(root: pathlib.Path) -> str:
    path = root / "plugins" / PLUGIN_NAME / ".codex-plugin" / "plugin.json"
    try:
        return json.loads(path.read_text()).get("version", "unknown")
    except (OSError, json.JSONDecodeError):
        return "missing"


def worker_details() -> tuple[str, str, str]:
    launchctl = os.environ.get("LAUNCHCTL_BIN", "launchctl")
    result = run([launchctl, "print", f"gui/{os.getuid()}/{WORKER_LABEL}"], check=False)
    state = "not loaded"
    pid = "-"
    if result.returncode == 0:
        for raw in result.stdout.splitlines():
            line = raw.strip()
            if line.startswith("state =") and state == "not loaded":
                state = line.split("=", 1)[1].strip()
            if line.startswith("pid ="):
                pid = line.split("=", 1)[1].strip()
                break
    plist = pathlib.Path.home() / "Library" / "LaunchAgents" / f"{WORKER_LABEL}.plist"
    binary = "missing"
    try:
        with plist.open("rb") as handle:
            arguments = plistlib.load(handle).get("ProgramArguments", [])
        if arguments:
            binary = arguments[0]
    except (OSError, plistlib.InvalidFileException):
        pass
    build = "unknown"
    if binary != "missing" and pathlib.Path(binary).is_file():
        go = os.environ.get("GO_BIN", "go")
        info = run([go, "version", "-m", binary], check=False)
        revision = ""
        modified = False
        for raw in info.stdout.splitlines():
            line = raw.strip()
            if line.startswith("build\tvcs.revision="):
                revision = line.split("=", 1)[1]
            elif line == "build\tvcs.modified=true":
                modified = True
        if revision:
            build = revision[:12] + ("+modified" if modified else "")
    return f"{state} pid={pid}", binary, build


def status() -> int:
    root = repo_root()
    source = ""
    if pointer_path().exists():
        source = pointer_path().read_text().strip()
    server = explicit_server()
    linked = is_our_server(server)
    custom = bool(server) and not linked
    commit, dirty = git_details(root)
    version = repo_version(root)
    plugin = plugin_details()
    mcp = mcp_details()
    worker, worker_binary, worker_build = worker_details()
    warnings: list[str] = []
    if linked:
        mode = "development"
        if not source or not pathlib.Path(source).is_dir():
            warnings.append("linked checkout is missing or moved")
        elif pathlib.Path(source).resolve() != root:
            warnings.append(f"linked checkout differs from this repo: {source}")
    elif custom:
        mode = "custom override"
        warnings.append("custom MCP override hides the production plugin MCP")
    else:
        mode = "production"
    installed_version = plugin.get("version", "missing") if plugin else "missing"
    installed_source = plugin.get("source", {}).get("path", "missing") if plugin else "missing"
    if installed_version != version:
        warnings.append(f"plugin version mismatch: repo={version} installed={installed_version}")
    transport = (mcp or {}).get("transport", {})
    transport_type = transport.get("type", "missing")
    entrypoint = transport.get("command") or transport.get("url") or "missing"
    cwd = transport.get("cwd") or (source if linked else "-")
    print(f"mode: {mode}")
    print(f"repo: {root}")
    print(f"repo version: {version}")
    print(f"repo commit: {commit}{' dirty' if dirty else ''}")
    print(f"installed plugin: {installed_version}")
    print(f"installed source: {installed_source}")
    print(f"MCP transport: {transport_type}")
    print(f"MCP entrypoint: {entrypoint}")
    print(f"MCP cwd/source: {cwd}")
    print(f"worker: {worker}")
    print(f"worker binary: {worker_binary}")
    print(f"worker build: {worker_build}")
    if warnings:
        for warning in warnings:
            print(f"warning: {warning}")
        return 1
    print("sync: ok")
    return 0


def main() -> int:
    if len(sys.argv) != 2 or sys.argv[1] not in {"link", "status", "unlink"}:
        print("usage: codex_plugin_dev.py <link|status|unlink>", file=sys.stderr)
        return 2
    try:
        if sys.argv[1] == "link":
            link()
        elif sys.argv[1] == "unlink":
            unlink()
        else:
            return status()
    except (OSError, RuntimeError, subprocess.CalledProcessError) as error:
        print(f"telegram-bridge dev setup: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
