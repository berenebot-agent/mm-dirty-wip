#!/bin/sh
set -eu
DATA_ROOT="${DATA_DIR:-/data}"
if [ "$(id -u)" = "0" ]; then
  mkdir -p "$DATA_ROOT"
  chown 65532:65532 "$DATA_ROOT"
  exec gosu 65532:65532 /usr/local/bin/open-agent-inbox "$@"
fi
exec /usr/local/bin/open-agent-inbox "$@"
