# syntax=docker/dockerfile:1
#
# Dedicated image for the remote MX edge (dialmx/cmd/receiver): the SMTP edge
# and the HTTPS/2 dialer session endpoint in one process. One image serves both
# modes — `DIALMX_MODE=shared` (DNS TXT domain proofs) and `DIALMX_MODE=single`
# (a shared bearer key). It is pure Go with no cgo and opens no application
# database, so it builds static and runs unprivileged. It needs DNS
# (/etc/resolv.conf) for SPF/DKIM/DMARC. It holds no /data mount and no
# APP_ENCRYPTION_KEY: it stages messages in memory and hands them to the core.
#
# Build from the repository root so the whole module is in context:
#   docker build -f dialmx/Dockerfile -t mammothmail-remote-mx .
#
# See docs/REMOTE_MX.md for operator setup.

FROM golang:1.27-bookworm AS build
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=$GOPROXY
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mammothmail-remote-mx ./dialmx/cmd/receiver

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata && rm -rf /var/lib/apt/lists/*

# The receiver holds no /data, no app key and needs no privilege; it stages
# messages in memory. Run it as the same non-root uid the other images use.
ARG MAMMOTHMAIL_UID=65532
ARG MAMMOTHMAIL_GID=65532
RUN groupadd -g ${MAMMOTHMAIL_GID} mammothmail 2>/dev/null || true \
 && useradd -u ${MAMMOTHMAIL_UID} -g ${MAMMOTHMAIL_GID} -M -s /usr/sbin/nologin mammothmail

COPY --from=build /out/mammothmail-remote-mx /usr/local/bin/mammothmail-remote-mx
COPY LICENSE THIRD_PARTY_NOTICES.md /usr/local/share/doc/mammothmail/
USER ${MAMMOTHMAIL_UID}:${MAMMOTHMAIL_GID}
# Inside the container the SMTP edge listens on 2525 and the session listener on
# 8443; map host 25 and 443 onto them. Both are unprivileged, so the process
# needs no capabilities even when the host ports are privileged.
EXPOSE 2525 8443
ENTRYPOINT ["/usr/local/bin/mammothmail-remote-mx"]
