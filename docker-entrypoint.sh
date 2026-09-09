#!/bin/sh
set -eu
DATA_ROOT="${DATA_DIR:-/data}"
# The container may start as root and drop privileges internally, but must not
# chown or require special ownership changes on the host ./data directory. The
# application creates its subdirectories (messages/.tmp) with 0700 as needed,
# so we simply exec the binary and let it manage its own data layout.
mkdir -p "$DATA_ROOT"
exec /usr/local/bin/gatehouse-mail "$@"
