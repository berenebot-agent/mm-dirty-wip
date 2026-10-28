# syntax=docker/dockerfile:1

FROM golang:1.27-bookworm AS build
RUN apt-get update && apt-get install -y --no-install-recommends libsqlite3-dev && rm -rf /var/lib/apt/lists/*
# MAMMOTHMAIL_BUILD stamps the running binary's identity, reported by /healthz and
# the discovery document. Pass the commit being deployed:
#   MAMMOTHMAIL_BUILD=$(git rev-parse --short HEAD) docker compose up -d --build
# When it is empty the image records its build timestamp instead, so a deployed
# container is still identifiable.
ARG MAMMOTHMAIL_BUILD=""
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=$GOPROXY
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 go build -trimpath -buildvcs=false \
    -ldflags="-s -w -X main.buildVersion=${MAMMOTHMAIL_BUILD:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}" \
    -o /out/mammothmail ./cmd/server
# The optional included MX edge is managed through persisted receiver settings.
# Remote receivers use the separate, minimal image in dialmx/Dockerfile.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/mammothmail-mx ./dialmx/cmd/receiver

FROM debian:bookworm-slim AS runtime
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata libsqlite3-0 && rm -rf /var/lib/apt/lists/*

# The image has no USER: it boots as root so the in-process privilege drop can
# chown a fresh root-owned ./data bind mount and then shed privileges to the
# runtime user before the database is opened. Hardened deployments override
# with `user:` in compose; the build args set the image-level /data ownership.
ARG MAMMOTHMAIL_UID=65532
ARG MAMMOTHMAIL_GID=65532
RUN mkdir -p /data && chown ${MAMMOTHMAIL_UID}:${MAMMOTHMAIL_GID} /data
COPY LICENSE THIRD_PARTY_NOTICES.md /usr/local/share/doc/mammothmail/

COPY --from=build /out/mammothmail /usr/local/bin/mammothmail
COPY --from=build /out/mammothmail-mx /usr/local/bin/mammothmail-mx
VOLUME ["/data"]
EXPOSE 8081 8082
ENTRYPOINT ["/usr/local/bin/mammothmail"]
