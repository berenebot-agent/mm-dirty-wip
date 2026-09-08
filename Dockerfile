FROM golang:1.23-bookworm AS build
RUN apt-get update && apt-get install -y --no-install-recommends libsqlite3-dev && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=1 go test ./... && CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/open-agent-inbox ./cmd/server

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata libsqlite3-0 gosu && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/open-agent-inbox /usr/local/bin/open-agent-inbox
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh
VOLUME ["/data"]
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
