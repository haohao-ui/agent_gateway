#!/usr/bin/env bash
set -euo pipefail
# Usage: scripts/build-release.sh VERSION SEQUENCE /offline/release.key /trusted/release.pub OUTPUT_DIR
VERSION="${1:?version required}"
SEQUENCE="${2:?sequence required}"
SIGNING_KEY="${3:?offline signing key path required}"
PUBLIC_KEY="${4:?independently trusted public key path required}"
OUTPUT="${5:?new release directory required}"
case "$VERSION" in *[!a-zA-Z0-9._-]*|'') echo 'Invalid version' >&2; exit 1;; esac
case "$SEQUENCE" in *[!0-9]*|'') echo 'Invalid sequence' >&2; exit 1;; esac
command -v govulncheck >/dev/null || { echo 'Install a pinned govulncheck release before signing.' >&2; exit 1; }
mkdir "$OUTPUT"
{
 go version
 govulncheck -version
 go vet ./...
 go test ./...
 go test -race ./...
 govulncheck ./...
} > "$OUTPUT/release-checks.log" 2>&1
python3 scripts/collect-notices.py
cp LICENSE NOTICE THIRD_PARTY_NOTICES README.md "$OUTPUT/"
COMMIT=$(git rev-parse --short HEAD)
BUILD_TIME=$(date -u +%Y-%m-%dT%H:%M:%SZ)
for TARGET in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
 OS=${TARGET%/*}; ARCH=${TARGET#*/}; SUFFIX=''
 if [ "$OS" = windows ]; then SUFFIX='.exe'; fi
 CGO_ENABLED=0 GOOS="$OS" GOARCH="$ARCH" go build -trimpath -ldflags "-X agent-gateway/internal/protocol.Version=$VERSION -X agent-gateway/internal/protocol.GitCommit=$COMMIT -X agent-gateway/internal/protocol.BuildTime=$BUILD_TIME" -o "$OUTPUT/mesh-$OS-$ARCH$SUFFIX" ./cmd/mesh
done
go run ./cmd/mesh release sign --dir "$OUTPUT" --key "$SIGNING_KEY" --version "$VERSION" --sequence "$SEQUENCE"
go run ./cmd/mesh release verify --dir "$OUTPUT" --public-key "$PUBLIC_KEY"
