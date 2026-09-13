# syntax=docker/dockerfile:1

FROM golang:1.27-bookworm AS build
RUN apt-get update && apt-get install -y --no-install-recommends libsqlite3-dev && rm -rf /var/lib/apt/lists/*
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=$GOPROXY
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/gatehouse-mail ./cmd/server
# The optional MX edge is built into the same image so the embedded mode
# (MX_ENABLE=local) can spawn it. The sidecar/remote deployments use the
# separate, minimal gatehouse-mx image instead (Dockerfile.mx), and select it
# with MX_ENABLE=remote.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/gatehouse-mx ./cmd/mx

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata libsqlite3-0 && rm -rf /var/lib/apt/lists/*

# The image has no USER: it boots as root so the in-process privilege drop can
# chown a fresh root-owned ./data bind mount and then shed privileges to the
# runtime user before the database is opened. Hardened deployments override
# with `user:` in compose; the build args set the image-level /data ownership.
ARG GATEHOUSE_UID=65532
ARG GATEHOUSE_GID=65532
RUN mkdir -p /data && chown ${GATEHOUSE_UID}:${GATEHOUSE_GID} /data

COPY --from=build /out/gatehouse-mail /usr/local/bin/gatehouse-mail
COPY --from=build /out/gatehouse-mx /usr/local/bin/gatehouse-mx
COPY LICENSE THIRD_PARTY_NOTICES.md /usr/local/share/doc/gatehouse-mail/
VOLUME ["/data"]
EXPOSE 8081 8082
ENTRYPOINT ["/usr/local/bin/gatehouse-mail"]
