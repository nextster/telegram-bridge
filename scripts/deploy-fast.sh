#!/usr/bin/env bash
set -euo pipefail

APP="${APP:-tg-radar}"
MODE="${MODE:-local}"
RUN_TESTS="${RUN_TESTS:-0}"
WAIT_HEALTH="${WAIT_HEALTH:-0}"
PACK="${PACK:-1}"
UPX_LEVEL="${UPX_LEVEL:-1}"
PUBLIC_URL="${PUBLIC_URL:-https://${APP}.fly.dev}"
TARGET_GOOS="${TARGET_GOOS:-linux}"
TARGET_GOARCH="${TARGET_GOARCH:-amd64}"
PLATFORM="${PLATFORM:-linux/amd64}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGE_TAG="${IMAGE_TAG:-fast-$(date -u +%Y%m%d%H%M%S)}"
IMAGE="${IMAGE:-registry.fly.io/${APP}:${IMAGE_TAG}}"

step_started_at=0

start_step() {
  step_started_at="$(date +%s)"
  printf '==> %s\n' "$1"
}

end_step() {
  local now
  now="$(date +%s)"
  printf '    done in %ss\n' "$((now - step_started_at))"
}

first_machine_id() {
  fly machine list --app "$APP" --json | jq -r '[.[] | select(.state != "destroyed")][0].id'
}

wait_for_machine() {
  local machine_id="$1"
  local deadline=$((SECONDS + 60))
  local state=""
  local current_image=""

  while (( SECONDS < deadline )); do
    local machines
    machines="$(fly machine list --app "$APP" --json)"
    state="$(jq -r --arg id "$machine_id" '.[] | select(.id == $id) | .state' <<<"$machines")"
    current_image="$(jq -r --arg id "$machine_id" '.[] | select(.id == $id) | .config.image' <<<"$machines")"
    if [[ "$state" == "started" && "$current_image" == "$IMAGE"* ]]; then
      curl -fsS "${PUBLIC_URL}/healthz"
      printf '\n'
      return 0
    fi
    sleep 1
  done

  printf 'machine did not report image=%s state=started in time; last state=%s image=%s\n' "$IMAGE" "$state" "$current_image" >&2
  return 1
}

wait_for_public_health() {
  local deadline=$((SECONDS + 60))
  while (( SECONDS < deadline )); do
    if curl -fsS "${PUBLIC_URL}/healthz"; then
      printf '\n'
      return 0
    fi
    sleep 1
  done

  printf 'health endpoint did not pass in time: %s/healthz\n' "$PUBLIC_URL" >&2
  return 1
}

cd "$ROOT"
mkdir -p .fly-build

total_started_at="$(date +%s)"

if [[ "$RUN_TESTS" == "1" ]]; then
  start_step "test"
  go test ./...
  end_step
fi

start_step "build ${TARGET_GOOS}/${TARGET_GOARCH}"
GOOS="$TARGET_GOOS" GOARCH="$TARGET_GOARCH" CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" -o .fly-build/tg-radar.raw ./cmd/tg-radar
end_step

if [[ "$PACK" == "1" ]]; then
  start_step "pack binary with upx -${UPX_LEVEL}"
  if ! command -v upx >/dev/null 2>&1; then
    printf 'upx not found; install it or run with PACK=0\n' >&2
    exit 1
  fi
  upx -q "-${UPX_LEVEL}" -o .fly-build/tg-radar --force-overwrite .fly-build/tg-radar.raw
  end_step
else
  cp .fly-build/tg-radar.raw .fly-build/tg-radar
fi

case "$MODE" in
  local)
    start_step "local fly deploy"
    fly deploy \
      --config fly.fast.toml \
      --local-only \
      --strategy immediate \
      --detach \
      --dns-checks=false \
      --smoke-checks=false \
      --ha=false \
      --compression zstd \
      --compression-level 1 \
      --yes \
      --wait-timeout=30s
    end_step
    ;;
  machine)
    start_step "auth registry.fly.io"
    fly auth docker >/dev/null
    end_step

    start_step "build and push ${IMAGE}"
    docker buildx build \
      --platform "$PLATFORM" \
      --file Dockerfile.fast \
      --tag "$IMAGE" \
      --push \
      --provenance=false \
      .
    end_step

    MACHINE_ID="${MACHINE_ID:-$(first_machine_id)}"
    if [[ -z "$MACHINE_ID" || "$MACHINE_ID" == "null" ]]; then
      printf 'no Fly machine found for app %s\n' "$APP" >&2
      exit 1
    fi
    start_step "update machine ${MACHINE_ID}"
    fly machine update "$MACHINE_ID" \
      --app "$APP" \
      --image "$IMAGE" \
      --skip-health-checks \
      --detach \
      --yes
    end_step
    ;;
  deploy)
    start_step "auth registry.fly.io"
    fly auth docker >/dev/null
    end_step

    start_step "build and push ${IMAGE}"
    docker buildx build \
      --platform "$PLATFORM" \
      --file Dockerfile.fast \
      --tag "$IMAGE" \
      --push \
      --provenance=false \
      .
    end_step

    start_step "deploy image"
    fly deploy \
      --app "$APP" \
      --image "$IMAGE" \
      --strategy immediate \
      --detach \
      --dns-checks=false \
      --smoke-checks=false \
      --ha=false \
      --yes \
      --wait-timeout=30s
    end_step
    ;;
  *)
    printf 'unknown MODE=%s, expected local, machine, or deploy\n' "$MODE" >&2
    exit 1
    ;;
esac

if [[ "$WAIT_HEALTH" == "1" ]]; then
  start_step "wait for health"
  if [[ "$MODE" == "local" ]]; then
    wait_for_public_health
  else
    wait_for_machine "${MACHINE_ID:-$(first_machine_id)}"
  fi
  end_step
fi

total_finished_at="$(date +%s)"
if [[ "$MODE" != "local" ]]; then
  printf 'image=%s\n' "$IMAGE"
fi
printf 'total=%ss\n' "$((total_finished_at - total_started_at))"
