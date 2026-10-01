# syntax=docker/dockerfile:1
#
# Dedicated image for the standalone Dial MX receiver (dialmx/cmd/receiver):
# the SMTP edge and the HTTPS/2 dialer session endpoint in one process. It is
# pure Go with no cgo and opens no application database, so it builds static and
# runs unprivileged. It needs DNS
# (/etc/resolv.conf) for SPF/DKIM/DMARC. It holds no /data mount and no
# APP_ENCRYPTION_KEY: it stages messages in memory and hands them to the core.
#
# Build from the repository root so the whole module is in context:
#   docker build -f dialmx/Dockerfile -t mailmoose-dialmx .
#
# See docs/DIALMX.md.

FROM golang:1.27-bookworm AS build
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=$GOPROXY
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mailmoose-dialmx ./dialmx/cmd/receiver

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata && rm -rf /var/lib/apt/lists/*

# The receiver holds no /data, no app key and needs no privilege; it stages
# messages in memory. Run it as the same non-root uid the other images use.
ARG MAILMOOSE_UID=65532
ARG MAILMOOSE_GID=65532
RUN groupadd -g ${MAILMOOSE_GID} mailmoose 2>/dev/null || true \
 && useradd -u ${MAILMOOSE_UID} -g ${MAILMOOSE_GID} -M -s /usr/sbin/nologin mailmoose

COPY --from=build /out/mailmoose-dialmx /usr/local/bin/mailmoose-dialmx
COPY LICENSE THIRD_PARTY_NOTICES.md /usr/local/share/doc/mailmoose/
USER ${MAILMOOSE_UID}:${MAILMOOSE_GID}
# Inside the container the SMTP edge listens on 2525 and the session listener on
# 8443; map host 25 and 443 onto them. Both are unprivileged, so the process
# needs no capabilities even when the host ports are privileged.
EXPOSE 2525 8443
ENTRYPOINT ["/usr/local/bin/mailmoose-dialmx"]
