#!/usr/bin/env bash
set -euo pipefail
umask 077
# Use a locally trusted verifier and keys, never a verifier/key fetched from this server.
: "${MESH_VERIFY_BIN:?Set MESH_VERIFY_BIN to an independently trusted mesh executable}"
: "${MESH_RELEASE_KEY:?Set MESH_RELEASE_KEY to the pinned release.pub file}"
: "${MESH_TLS_CA:?Set MESH_TLS_CA to the independently trusted gateway CA file}"
SERVER="${1:?HTTPS gateway URL required}"
TOKEN_FILE="${2:?Invitation token file required}"
DIR="${3:-$HOME/.agent-mesh-node}"
case "$SERVER" in https://*) ;; *) echo 'HTTPS required' >&2; exit 1;; esac
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in x86_64) ARCH=amd64;; arm64|aarch64) ARCH=arm64;; *) echo 'Unsupported architecture' >&2; exit 1;; esac
mkdir -p "$DIR"
"$MESH_VERIFY_BIN" release fetch --server "$SERVER" --ca "$MESH_TLS_CA" --public-key "$MESH_RELEASE_KEY" --target "$OS-$ARCH" --out "$DIR/mesh"
for name in LICENSE NOTICE THIRD_PARTY_NOTICES; do
 "$MESH_VERIFY_BIN" release fetch --server "$SERVER" --ca "$MESH_TLS_CA" --public-key "$MESH_RELEASE_KEY" --target "$name" --out "$DIR/$name"
done
# Pair only after signature, file digest and platform checks have all succeeded.
"$DIR/mesh" pair --server "$SERVER" --ca "$MESH_TLS_CA" --token - --dir "$DIR" < "$TOKEN_FILE"
cp -n "$MESH_RELEASE_KEY" "$DIR/release.pub"
echo 'Verified installation and pairing complete. Review node.json before starting the node.'
