from __future__ import annotations

import json
import os
import pathlib
import subprocess
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "sync-nextster-marketplace.py"


class SyncNextsterMarketplaceTest(unittest.TestCase):
    def test_sync_preserves_other_plugins(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            marketplace_root = pathlib.Path(temporary) / "marketplace"
            manifest = marketplace_root / ".agents" / "plugins" / "marketplace.json"
            figma = marketplace_root / "plugins" / "figma-bridge"
            manifest.parent.mkdir(parents=True)
            figma.mkdir(parents=True)
            (figma / "marker.txt").write_text("keep")
            manifest.write_text(json.dumps({
                "name": "nextster",
                "interface": {"displayName": "Nextster"},
                "plugins": [{
                    "name": "figma-bridge",
                    "source": {"source": "local", "path": "./plugins/figma-bridge"},
                }],
            }))
            subprocess.run(
                ["python3", str(SCRIPT)],
                check=True,
                capture_output=True,
                text=True,
                env={**os.environ, "NEXTSTER_MARKETPLACE_DIR": str(marketplace_root)},
            )
            payload = json.loads(manifest.read_text())
            self.assertEqual([entry["name"] for entry in payload["plugins"]], ["figma-bridge", "telegram-bridge"])
            self.assertEqual((figma / "marker.txt").read_text(), "keep")
            self.assertTrue((marketplace_root / "plugins" / "telegram-bridge" / ".mcp.json").is_file())


if __name__ == "__main__":
    unittest.main()
