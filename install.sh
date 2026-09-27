#!/bin/sh
# Agent Gateway 安装脚本（macOS / Linux）。
#
# 一键安装（使用发布页附带的脚本，内嵌该版本各平台二进制的 SHA-256）：
#   curl -fsSL https://github.com/haohao-ui/agent_gateway/releases/latest/download/install.sh | sh
#
# 带外公钥（本机有验签后端时做完整签名验证，不依赖脚本内嵌指纹）：
#   sh install.sh --public-key /trusted/release.pub
#
# 独立验证器（最强路径）：
#   sh install.sh --verifier /trusted/mesh --public-key /trusted/release.pub
#
# 信任模型：
#   一键路径的信任锚是 HTTPS 传输 + 脚本内嵌的 SHA-256 指纹，指纹取自该版本的已签名
#   清单。本机存在可用后端（支持 Ed25519 的 openssl，或带 cryptography 的 python3）时，
#   脚本会额外用内嵌公钥验签，并要求签名清单里的目标条目与下载到的二进制一致，然后把
#   校验等级提升为完整验签。需要不依赖下载服务器的保证时，请带外提供公钥（--public-key）
#   或用 --fingerprint 固定公钥指纹。
#
# 不提供跳过校验的选项。
set -eu
umask 077

VERSION=${AGENT_GATEWAY_VERSION:-latest}
DIR=${AGENT_GATEWAY_INSTALL_DIR:-"$HOME/.local/bin"}
REPO=${AGENT_GATEWAY_REPO:-haohao-ui/agent_gateway}
BASE_URL=${AGENT_GATEWAY_BASE_URL:-}
VERIFIER=${AGENT_GATEWAY_VERIFY_BIN:-}
PUBLIC_KEY=${AGENT_GATEWAY_PUBLIC_KEY:-}
FINGERPRINT=${AGENT_GATEWAY_FINGERPRINT:-}

# >>> PINNED（发版时由 scripts/pin-installers.py 注入；仓库副本保持为空）
PINNED_VERSION=""
PINNED_PUBLIC_KEY=""
PINNED_darwin_amd64=""
PINNED_darwin_arm64=""
PINNED_linux_amd64=""
PINNED_linux_arm64=""
# <<< PINNED

usage() {
	cat <<'EOF'
用法：sh install.sh [选项]

一键安装：不带选项即可，使用脚本内嵌的版本指纹。

带外公钥 / 独立验证器：
  --public-key <路径>    带外获取的发行公钥（配合本机验签后端）
  --verifier <路径>      独立可信的 mesh 验证器（最强路径，需与 --public-key 同用）

其它：
  --fingerprint <sha256> 要求公钥指纹必须匹配（带外固定）
  --version <vX.Y.Z>     指定版本（默认最新正式版）
  --dir <路径>           安装目录（默认 ~/.local/bin）
  --repo <owner/repo>    指定仓库
  --base-url <URL>       指定下载基址（镜像站；必须为 https）
  -h, --help             显示本帮助
EOF
}

die() {
	printf '错误：%s\n' "$1" >&2
	exit 1
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	--version)
		VERSION=${2:?version required}
		shift 2
		;;
	--dir)
		DIR=${2:?directory required}
		shift 2
		;;
	--repo)
		REPO=${2:?repository required}
		shift 2
		;;
	--base-url)
		BASE_URL=${2:?base URL required}
		shift 2
		;;
	--verifier)
		VERIFIER=${2:?trusted verifier required}
		shift 2
		;;
	--public-key)
		PUBLIC_KEY=${2:?trusted key required}
		shift 2
		;;
	--fingerprint)
		FINGERPRINT=${2:?fingerprint required}
		shift 2
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		echo '未知或不安全的参数；不支持跳过校验。' >&2
		usage >&2
		exit 1
		;;
	esac
done

case "$VERSION" in *[!A-Za-z0-9._-]* | '') die "版本号非法：$VERSION" ;; esac

# 1. 平台探测
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
MACHINE=$(uname -m)
case "$MACHINE" in
x86_64 | amd64) ARCH=amd64 ;;
arm64 | aarch64) ARCH=arm64 ;;
*) die "不支持的架构：$MACHINE" ;;
esac
case "$OS" in
darwin | linux) ;;
*) die "不支持的系统：${OS}（Windows 请使用 install.ps1）" ;;
esac
TARGET="${OS}-${ARCH}"
ASSET="mesh-${TARGET}"

case "$TARGET" in
darwin-amd64) PINNED_HASH=$PINNED_darwin_amd64 ;;
darwin-arm64) PINNED_HASH=$PINNED_darwin_arm64 ;;
linux-amd64) PINNED_HASH=$PINNED_linux_amd64 ;;
linux-arm64) PINNED_HASH=$PINNED_linux_arm64 ;;
*) PINNED_HASH="" ;;
esac

# 2. 模式判定
MODE=pinned
if [ -n "$VERIFIER" ]; then
	[ -x "$VERIFIER" ] || die "找不到可执行的验证器：${VERIFIER}"
	[ -n "$PUBLIC_KEY" ] && [ -r "$PUBLIC_KEY" ] || die "使用 --verifier 时必须同时提供可读的 --public-key"
	MODE=strong
elif [ -n "$PUBLIC_KEY" ]; then
	[ -r "$PUBLIC_KEY" ] || die "找不到可读的发行公钥：${PUBLIC_KEY}"
	MODE=key
elif [ -z "$PINNED_HASH" ]; then
	die "此副本没有内嵌 ${TARGET} 的指纹。请使用发布页附带的 install.sh，或用 --public-key / --verifier 指定带外公钥与独立验证器。"
fi

# 3. 下载地址
if [ -z "$BASE_URL" ]; then
	if [ "$VERSION" = latest ]; then
		BASE_URL="https://github.com/${REPO}/releases/latest/download"
	else
		BASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"
	fi
fi
case "$BASE_URL" in
https://*) ;;
*) die "下载基址必须是 https：${BASE_URL}" ;;
esac
BASE_URL=${BASE_URL%/}

command -v curl >/dev/null 2>&1 || die "需要 curl"

TMP=$(mktemp -d)
STAGED=""
cleanup() {
	rm -rf "$TMP"
	[ -n "$STAGED" ] && rm -f "$STAGED"
	return 0
}
trap 'cleanup' EXIT HUP INT TERM

printf '平台：%s\n' "$TARGET"
printf '来源：%s\n' "$BASE_URL"

# curl 只允许 https、限制重定向协议与体积，与节点自升级的下载约束一致。
fetch() {
	curl --proto '=https' --proto-redir '=https' --tlsv1.2 --fail --silent --show-error \
		--location --max-time 120 --max-filesize 268435456 "$1" --output "$2"
}

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
	elif command -v openssl >/dev/null 2>&1; then
		openssl dgst -sha256 "$1" | awk '{print $NF}'
	else
		die "缺少 sha256sum / shasum / openssl，无法校验完整性"
	fi
}

# 输出可用验签后端名；没有则返回非 0。能力探测与"签名无效"区分开：
# LibreSSL 的 pkeyutl 没有 -rawin，因此不会被误判为可用的 Ed25519 验证器。
have_backend() {
	if command -v openssl >/dev/null 2>&1 && openssl pkeyutl -help 2>&1 | grep -q -- '-rawin'; then
		echo openssl
		return 0
	fi
	if command -v python3 >/dev/null 2>&1 &&
		python3 -c 'import cryptography.hazmat.primitives.asymmetric.ed25519' >/dev/null 2>&1; then
		echo python3
		return 0
	fi
	return 1
}

# $1=目录 $2=公钥；用可用后端验证 manifest.json 的签名。
verify_manifest() {
	backend=$(have_backend) || return 1
	if [ "$backend" = openssl ]; then
		openssl pkeyutl -verify -pubin -inkey "$2" -rawin \
			-in "$1/manifest.json" -sigfile "$1/manifest.sig" >/dev/null 2>&1
		return $?
	fi
	python3 - "$1" "$2" <<'PY'
import sys
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import ed25519

directory, key_path = sys.argv[1], sys.argv[2]
with open(key_path, 'rb') as stream:
    public = serialization.load_pem_public_key(stream.read())
if not isinstance(public, ed25519.Ed25519PublicKey):
    raise SystemExit(1)
with open(f'{directory}/manifest.json', 'rb') as stream:
    message = stream.read()
with open(f'{directory}/manifest.sig', 'rb') as stream:
    signature = stream.read()
public.verify(signature, message)
PY
}

# $1=目录 $2=目标平台；从签名清单中取出该平台二进制的 SHA-256。
manifest_hash() {
	grep -A4 "\"$2\": {" "$1/manifest.json" 2>/dev/null | grep -o '[0-9a-f]\{64\}' | head -1
}

need_manifest() {
	[ -f "$TMP/manifest.json" ] && return 0
	fetch "$BASE_URL/manifest.json" "$TMP/manifest.json" || die "下载失败：manifest.json"
	fetch "$BASE_URL/manifest.sig" "$TMP/manifest.sig" || die "下载失败：manifest.sig"
}

# 4. 下载
printf '下载：%s/%s\n' "$BASE_URL" "$ASSET"
fetch "$BASE_URL/$ASSET" "$TMP/$ASSET" || die "下载失败：${BASE_URL}/${ASSET}"
for file in LICENSE NOTICE THIRD_PARTY_NOTICES; do
	fetch "$BASE_URL/$file" "$TMP/$file" || die "下载失败：${BASE_URL}/${file}"
done

# 5. 校验
ACTUAL=$(sha256_of "$TMP/$ASSET")
case "$MODE" in
strong)
	need_manifest
	"$VERIFIER" release verify --dir "$TMP" --public-key "$PUBLIC_KEY" --target "$TARGET" ||
		die "独立验证器拒绝该发行包"
	if [ -n "$FINGERPRINT" ]; then
		KEYFP=$(sha256_of "$PUBLIC_KEY")
		[ "$KEYFP" = "$FINGERPRINT" ] || die "公钥指纹不匹配：期望 ${FINGERPRINT}，实际 ${KEYFP}"
	fi
	LEVEL="完整验签（独立验证器）"
	;;
key)
	BACKEND=$(have_backend) ||
		die "本机没有可用的验签后端（需要支持 Ed25519 的 openssl，或带 cryptography 的 python3）。请改用 --verifier 指定独立验证器。"
	if [ -n "$FINGERPRINT" ]; then
		KEYFP=$(sha256_of "$PUBLIC_KEY")
		[ "$KEYFP" = "$FINGERPRINT" ] || die "公钥指纹不匹配：期望 ${FINGERPRINT}，实际 ${KEYFP}"
	fi
	need_manifest
	verify_manifest "$TMP" "$PUBLIC_KEY" || die "清单签名验证失败：公钥与清单或签名不一致"
	EXPECTED=$(manifest_hash "$TMP" "$TARGET")
	[ -n "$EXPECTED" ] || die "签名清单中没有 ${TARGET} 的条目"
	[ "$ACTUAL" = "$EXPECTED" ] || die "SHA-256 与签名清单不符：期望 ${EXPECTED}，实际 ${ACTUAL}"
	if [ -n "$PINNED_HASH" ] && [ "$PINNED_HASH" != "$EXPECTED" ]; then
		die "脚本内嵌指纹与签名清单不一致"
	fi
	LEVEL="完整验签（带外公钥 + ${BACKEND}）"
	;;
pinned)
	[ "$ACTUAL" = "$PINNED_HASH" ] || die "SHA-256 校验失败：期望 ${PINNED_HASH}，实际 ${ACTUAL}"
	LEVEL="内嵌 SHA-256 指纹（来源：该版本的已签名清单，锚定 HTTPS 传输）"
	KEYFILE=""
	if [ -n "$PINNED_PUBLIC_KEY" ]; then
		KEYFILE="$TMP/pinned.pub"
		printf '%s\n' "$PINNED_PUBLIC_KEY" >"$KEYFILE"
	fi
	if [ -n "$FINGERPRINT" ]; then
		[ -n "$KEYFILE" ] || die "--fingerprint 需要脚本内嵌公钥（请使用发布页附带的 install.sh）"
		KEYFP=$(sha256_of "$KEYFILE")
		[ "$KEYFP" = "$FINGERPRINT" ] || die "公钥指纹不匹配：期望 ${FINGERPRINT}，实际 ${KEYFP}"
		LEVEL="${LEVEL}；公钥指纹已按 --fingerprint 固定"
	fi
	if [ -n "$KEYFILE" ]; then
		if backend=$(have_backend); then
			need_manifest
			verify_manifest "$TMP" "$KEYFILE" ||
				die "清单签名验证失败：内嵌公钥与下载到的清单或签名不一致"
			EXPECTED=$(manifest_hash "$TMP" "$TARGET")
			[ -n "$EXPECTED" ] && [ "$EXPECTED" = "$ACTUAL" ] ||
				die "签名清单中的 ${TARGET} 与下载到的二进制不一致"
			LEVEL="${LEVEL}；清单签名验证通过（${backend}）"
		fi
	fi
	;;
esac

# 6. 安装（哈希与验签全部通过后才赋予可执行权限并落盘）
mkdir -p "$DIR/agent-gateway-notices"
[ -L "$DIR/mesh" ] && die "拒绝覆盖符号链接：${DIR}/mesh"
STAGED=$(mktemp "$DIR/.mesh-install.XXXXXX")
cp "$TMP/$ASSET" "$STAGED"
chmod 755 "$STAGED"
[ -f "$DIR/mesh" ] && cp -p "$DIR/mesh" "$DIR/mesh.bak"
mv "$STAGED" "$DIR/mesh"
STAGED=""
for file in LICENSE NOTICE THIRD_PARTY_NOTICES; do
	cp "$TMP/$file" "$DIR/agent-gateway-notices/$file"
done

printf '\n校验等级：%s\n' "$LEVEL"
printf 'SHA-256：%s\n' "$ACTUAL"
printf '已安装：%s/mesh\n' "$DIR"
"$DIR/mesh" --version || true

case ":${PATH}:" in
*":${DIR}:"*) ;;
*)
	printf '\n提示：%s 不在 PATH 中，请加入 shell 配置，例如：\n' "$DIR"
	printf "  echo 'export PATH=\"%s:\$PATH\"' >> ~/.profile\n" "$DIR"
	;;
esac

INSTALL_URL="https://github.com/${REPO}/releases/latest/download/install.sh"
if [ -n "$VERSION" ] && [ "$VERSION" != latest ]; then
	INSTALL_URL="https://github.com/${REPO}/releases/download/${VERSION}/install.sh"
fi
PUBLISHED_FP='（本副本未内嵌公钥）'
if [ -f "$TMP/pinned.pub" ]; then
	PUBLISHED_FP=$(sha256_of "$TMP/pinned.pub")
fi

cat <<EOF

下一步：
  mesh server                          启动网关（首次运行自动生成 CA、证书与数据库）
  mesh credential issue --out op.token 签发操作员令牌
  mesh service install --role server   注册为后台服务（macOS launchd / Linux systemd）

需要不依赖下载服务器的完整验签保证时，把 <公钥文件> 换成你自己的 release.pub：
  curl -fsSL ${INSTALL_URL} | sh -s -- --public-key <公钥文件>

也可以把脚本保存下来反复使用：
  curl -fsSLO ${INSTALL_URL}
  sh install.sh --public-key <公钥文件>

本次脚本内嵌公钥指纹（可用 --fingerprint 固定核对）：
  ${PUBLISHED_FP}

文档：
  https://github.com/${REPO}/blob/main/docs/INSTALL.md
EOF
