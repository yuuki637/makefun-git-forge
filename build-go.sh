#!/bin/sh
# Builds the Go forge server from gosrc/ and writes forge.gz (gzip -9 of
# the stripped static binary) at the repo root.
set -eu
cd "$(dirname "$0")"

GO="${GO:-}"
if [ -z "$GO" ]; then
  if [ -x "$HOME/workspace/tools/go/bin/go" ]; then
    GO="$HOME/workspace/tools/go/bin/go"
  else
    GO="$(command -v go)"
  fi
fi

export GOPATH="${GOPATH:-$HOME/workspace/tools/gopath}"
export GOCACHE="${GOCACHE:-$HOME/workspace/tools/gocache}"
export GOFLAGS="-mod=mod"
export CGO_ENABLED=0

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
(cd gosrc && "$GO" build -ldflags="-s -w" -trimpath -o "$tmp/forge" .)
gzip -9 -c "$tmp/forge" > forge.gz
echo "forge.gz: $(wc -c < forge.gz) bytes (raw binary: $(wc -c < "$tmp/forge") bytes)"
