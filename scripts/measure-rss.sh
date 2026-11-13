#!/usr/bin/env bash
set -euo pipefail

BINARY="./bin/kelvios-core"
PORT=8080
HEALTHZ_URL="http://localhost:${PORT}/healthz"
MAX_WAIT=30

if [[ ! -f "$BINARY" ]]; then
    echo "Binary not found: $BINARY" >&2
    echo "Run 'make build' first" >&2
    exit 1
fi

cleanup() {
    if [[ -n "${PID:-}" ]]; then
        kill "$PID" 2>/dev/null || true
        wait "$PID" 2>/dev/null || true
    fi
}
trap cleanup EXIT

"$BINARY" &
PID=$!

echo "Waiting for /healthz to be ready..."
for i in $(seq 1 "$MAX_WAIT"); do
    if curl -sf "$HEALTHZ_URL" >/dev/null 2>&1; then
        echo "Health endpoint ready"
        break
    fi
    sleep 1
    if [[ $i -eq $MAX_WAIT ]]; then
        echo "Timeout waiting for /healthz" >&2
        exit 1
    fi
done

RSS_KB=$(ps -o rss= -p "$PID" | awk '{print $1}')
echo "RSS: ${RSS_KB} KB"

curl -s "$HEALTHZ_URL"
echo