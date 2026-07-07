# tg-radar Agent Notes

## Deploy

For code-only redeploys, use:

```sh
scripts/deploy-fast.sh
```

Do not run bare `fly deploy --local-only` by hand for the fast path: it can pick
the regular `fly.toml`/`Dockerfile` path. The script builds the Linux/amd64 Go
binary locally, packs it with UPX, and deploys with `fly.fast.toml` and
`Dockerfile.fast`.

Use regular `fly deploy` after changing Fly infrastructure, mounts, services,
base images, or the normal Dockerfile.

For an extra public health check after fast deploy:

```sh
WAIT_HEALTH=1 scripts/deploy-fast.sh
```

## Verify

Before finishing code changes, run:

```sh
go test ./...
```
