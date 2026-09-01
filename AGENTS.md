# telegram-bridge Agent Notes

## Deploy

For code-only redeploys, use:

```sh
scripts/deploy-fast.sh
```

Do not run bare `fly deploy --local-only` by hand for the fast path: it can pick
the regular `fly.toml`/`Dockerfile` path and requires a Docker-compatible
daemon. The script builds the image with Apple `container`, exports its OCI
layout, uploads it with `regctl`, and deploys with `fly.fast.toml` and
`Dockerfile.fast`. Fly Machines currently run on x86_64, so production defaults
to `linux/amd64`; local ARM64 smoke images are supported through `TARGET_GOARCH`
and `PLATFORM`.

Use regular `fly deploy` after changing Fly infrastructure, mounts, services,
base images, or the normal Dockerfile.

For an extra public health check after fast deploy:

```sh
WAIT_HEALTH=1 scripts/deploy-fast.sh
```

## Verify

Before finishing code changes, run:

```sh
scripts/check.sh
```

## Codex plugin

The canonical plugin source is `plugins/telegram-bridge`, and the repository-local
marketplace manifest is `.agents/plugins/marketplace.json`. Do not edit a copy
under `~/plugins/telegram-bridge` as an independent source.

After plugin or skill changes, run:

```sh
scripts/codex-plugin.sh reload
```

This validates the skill and plugin, refreshes the tracked cachebuster, and
reinstalls from the repository marketplace. Commit the resulting manifest
version together with the plugin change.

For uncommitted MCP adapter development, prefer `scripts/codex-plugin.sh
dev:link`; then run `scripts/check.sh` and open a new task. Use `dev:status` to
inspect drift and `dev:unlink` to return to the installed production plugin.
This dev override must not replace the versioned production install flow.
