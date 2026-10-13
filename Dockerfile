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
    CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/mailmoose ./cmd/server
# The optional MX edge is built into the same image so the embedded mode
# (MX_ENABLE=true) can spawn it. The remote deployment uses the
# separate, minimal mailmoose-mx image instead (Dockerfile.mx), and selects it
# with MX_ENABLE=remote.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/mailmoose-mx ./dialmx/cmd/receiver
# The one-shot secret migrator (cmd/migrate-secrets) rewrites ciphertext
# written before stored secrets carried an AAD binding, so an instance created
# earlier keeps working. Built into the image so an operator needs no Go
# toolchain: docker compose run --rm --user 65532:65532 --entrypoint
# migrate-secrets mailmoose -data-dir /data. See docs/SELFHOSTING.md.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/migrate-secrets ./cmd/migrate-secrets

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata libsqlite3-0 && rm -rf /var/lib/apt/lists/*

# The image has no USER: it boots as root so the in-process privilege drop can
# chown a fresh root-owned ./data bind mount and then shed privileges to the
# runtime user before the database is opened. Hardened deployments override
# with `user:` in compose; the build args set the image-level /data ownership.
ARG MAILMOOSE_UID=65532
ARG MAILMOOSE_GID=65532
RUN mkdir -p /data && chown ${MAILMOOSE_UID}:${MAILMOOSE_GID} /data

COPY --from=build /out/mailmoose /usr/local/bin/mailmoose
COPY --from=build /out/mailmoose-mx /usr/local/bin/mailmoose-mx
# The one-shot secret migrator; see the build stage above.
COPY --from=build /out/migrate-secrets /usr/local/bin/migrate-secrets
COPY LICENSE THIRD_PARTY_NOTICES.md /usr/local/share/doc/mailmoose/
VOLUME ["/data"]
EXPOSE 8081 8082
ENTRYPOINT ["/usr/local/bin/mailmoose"]
