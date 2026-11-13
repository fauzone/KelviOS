#!/usr/bin/env bash
# Sync built webapp assets into web/dist for embedding.
# Run from repo root. Requires webapp/dist to exist (run `cd webapp && npm run build` first).

set -e

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WEBAPP_DIST="${REPO_ROOT}/webapp/dist"
WEB_DIST="${REPO_ROOT}/web/dist"

if [[ ! -f "${WEBAPP_DIST}/index.html" ]]; then
    echo "webapp belum di-build: ${WEBAPP_DIST}/index.html tidak ditemukan" >&2
    echo "Jalankan: cd webapp && npm run build" >&2
    exit 1
fi

# Remove old assets directory
rm -rf "${WEB_DIST}/assets"

# Copy new assets
cp "${WEBAPP_DIST}/index.html" "${WEB_DIST}/index.html"
cp -r "${WEBAPP_DIST}/assets" "${WEB_DIST}/assets"

echo "Synced webapp/dist -> web/dist"