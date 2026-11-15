#!/bin/sh
set -eu

if [ "$(id -u)" = 0 ]; then
    data_dir="${DATA_DIR:-/app/data}"
    mkdir -p "$data_dir"
    resolved="$(readlink -f "$data_dir")"
    # The old Python image assigned different IDs to its app user.
    # Only the application's default volume may be recursively re-owned.
    if [ "$resolved" = /app/data ]; then
        chown -Rh app:app "$resolved"
    fi
    exec su-exec app:app "$@"
fi

exec "$@"
