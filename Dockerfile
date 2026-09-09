# syntax=docker/dockerfile:1

FROM golang:1.23-bookworm AS build
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

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata libsqlite3-0 gosu && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/gatehouse-mail /usr/local/bin/gatehouse-mail
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh
COPY LICENSE THIRD_PARTY_NOTICES.md /usr/local/share/doc/gatehouse-mail/
VOLUME ["/data"]
EXPOSE 8081 8082
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
