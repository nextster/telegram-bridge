# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.26.4-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/telegram-bridge ./cmd/telegram-bridge

FROM alpine:3.22

RUN apk add --no-cache ca-certificates ffmpeg && adduser -D -H -u 10001 app
WORKDIR /app
COPY --from=build /out/telegram-bridge /usr/local/bin/telegram-bridge
RUN mkdir -p /data && chown app:app /data
USER app

ENV PORT=8080
EXPOSE 8080

CMD ["telegram-bridge", "serve"]
