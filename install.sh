#!/bin/sh
# Install only after authentication by an independently trusted mesh verifier.
set -eu
umask 077
VERSION=${AGENT_GATEWAY_VERSION:-latest}
DIR=${AGENT_GATEWAY_INSTALL_DIR:-"$HOME/.local/bin"}
REPO=${AGENT_GATEWAY_REPO:-haohao-ui/agent_gateway}
BASE_URL=${AGENT_GATEWAY_BASE_URL:-}
VERIFIER=${AGENT_GATEWAY_VERIFY_BIN:-}
PUBLIC_KEY=${AGENT_GATEWAY_PUBLIC_KEY:-}
while [ "$#" -gt 0 ]; do
 case "$1" in
  --version) VERSION=${2:?version required};shift 2;;
  --dir) DIR=${2:?directory required};shift 2;;
  --repo) REPO=${2:?repository required};shift 2;;
  --base-url) BASE_URL=${2:?base URL required};shift 2;;
  --verifier) VERIFIER=${2:?trusted verifier required};shift 2;;
  --public-key) PUBLIC_KEY=${2:?trusted key required};shift 2;;
  --help|-h) echo 'Usage: sh install.sh --verifier /trusted/mesh --public-key /trusted/release.pub [--version vX.Y.Z] [--dir DIR]';exit 0;;
  *) echo 'Unknown/unsafe argument; skipping verification is not supported.' >&2;exit 1;;
 esac
done
[ -n "$VERIFIER" ] && [ -x "$VERIFIER" ] && [ -n "$PUBLIC_KEY" ] && [ -r "$PUBLIC_KEY" ] || {
 echo 'An independently trusted mesh verifier and release public key are required.' >&2;exit 1;
}
case "$VERSION" in *[!A-Za-z0-9._-]*|'') echo 'Invalid version' >&2;exit 1;; esac
if [ -z "$BASE_URL" ]; then
 if [ "$VERSION" = latest ]; then BASE_URL="https://github.com/$REPO/releases/latest/download"
 else BASE_URL="https://github.com/$REPO/releases/download/$VERSION";fi
fi
case "$BASE_URL" in https://*) ;; *) echo 'HTTPS required' >&2;exit 1;; esac
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in x86_64) ARCH=amd64;; arm64|aarch64) ARCH=arm64;; *) echo 'Unsupported architecture' >&2;exit 1;; esac
case "$OS" in darwin|linux) ;; *) echo 'Use install.ps1 on Windows' >&2;exit 1;; esac
TARGET="$OS-$ARCH"
ASSET="mesh-$TARGET"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT HUP INT TERM
for file in manifest.json manifest.sig "$ASSET" LICENSE NOTICE THIRD_PARTY_NOTICES; do
 curl --proto '=https' --proto-redir '=https' --tlsv1.2 --fail --silent --show-error --location --max-time 90 --max-filesize 268435456 "$BASE_URL/$file" --output "$TMP/$file"
done
"$VERIFIER" release verify --dir "$TMP" --public-key "$PUBLIC_KEY" --target "$TARGET"
# No executable content is run, moved into place or granted execute permission before verification.
mkdir -p "$DIR/agent-gateway-notices"
if [ -L "$DIR/mesh" ]; then echo 'Refusing to replace a symlink' >&2;exit 1;fi
STAGED=$(mktemp "$DIR/.mesh-install.XXXXXX")
trap 'rm -rf "$TMP"; rm -f "$STAGED"' EXIT HUP INT TERM
cp "$TMP/$ASSET" "$STAGED"
chmod 755 "$STAGED"
if [ -f "$DIR/mesh" ]; then cp -p "$DIR/mesh" "$DIR/mesh.bak";fi
mv "$STAGED" "$DIR/mesh"
for file in LICENSE NOTICE THIRD_PARTY_NOTICES; do cp "$TMP/$file" "$DIR/agent-gateway-notices/$file";done
"$DIR/mesh" --version
echo "Installed verified mesh at $DIR/mesh. No services or shell configuration were changed."
