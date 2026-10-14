# telegram-bridge Agent Notes

## Deploy

For code-only redeploys, use:

```sh
scripts/deploy-fast.sh
```

First prepare the target runtime with `scripts/build-runtime.sh`, and repeat
after changing `Dockerfile.runtime`. This separate step requires a builder
that can execute the target architecture; `RUNTIME_BUILDER` selects it. The
fast Dockerfile only copies the cross-compiled binary and performs no `RUN`.
Missing runtime layouts stop the fast script before compilation or deployment.

Do not run bare `fly deploy --local-only` by hand for the fast path: it can pick
the regular `fly.toml`/`Dockerfile` path and requires a Docker-compatible
daemon. The script builds and pushes the image with Docker Buildx and deploys
with `fly.fast.toml` and `Dockerfile.fast`. Use a running Docker-compatible
daemon selected by the current Docker context or the standard `DOCKER_HOST` /
`DOCKER_CONTEXT` overrides. Fly Machines run on x86_64, so production defaults
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

The canonical plugin source is `plugins/telegram-bridge`, and its tracked
Nextster marketplace entry is `.agents/plugins/marketplace.json`. Installation
syncs the plugin into `~/.agent-plugins/nextster`; do not edit that generated
copy as an independent source.

After plugin or skill changes, run:

```sh
scripts/codex-plugin.sh reload
```

This validates the skill and plugin, refreshes the tracked cachebuster, syncs
the shared Nextster marketplace, and reinstalls from it. Commit the resulting manifest
version together with the plugin change.
