#!/usr/bin/env bash
# Agent Gateway 节点安装脚本（由网关分发）。
#
# 推荐用法（控制台会给出带哈希与指纹的完整命令，无需手动下载 CA）：
#   curl -kfsSL https://<网关>:8443/download/install.sh -o install.sh
#   echo "<脚本哈希>  install.sh" | sha256sum -c -
#   MESH_TOKEN='<邀请码>' MESH_CA_FINGERPRINT='<CA指纹>' sh install.sh https://<网关>:8443
#
# 也可以用控制台「下载 CA 证书」得到的文件，做完全不依赖 -k 的强校验：
#   curl --cacert ./ca.crt -fsSL https://<网关>:8443/download/install.sh | MESH_TOKEN='<邀请码>' bash -s -- https://<网关>:8443 --ca ./ca.crt
#
# 信任模型：
#   提供了 --ca 文件时，脚本与所有下载都用该 CA 校验网关 TLS。
#   只提供了 --ca-fingerprint 时，脚本先从网关取回 CA 并比对指纹（DER 的 SHA-256），
#   比对通过后再用该 CA 校验后续全部下载。指纹来自控制台页面，是这条链路的锚。
#   设置 MESH_VERIFY_BIN 与 MESH_RELEASE_KEY 可再要求独立验证器做发行签名验证。
#
# 邀请码只经环境变量或文件传递，不进入 URL 或命令行参数；配对时通过 stdin 交给 mesh。
set -euo pipefail
umask 077

DEFAULT_SERVER=""
DEFAULT_TOKEN=""
DEFAULT_CA_FP=""

SERVER="${DEFAULT_SERVER:-}"
CA="${MESH_TLS_CA:-${MESH_CA:-}}"
CA_FP="${DEFAULT_CA_FP:-${MESH_CA_FINGERPRINT:-}}"
TOKEN="${DEFAULT_TOKEN:-${MESH_TOKEN:-}}"
TOKEN_FILE=""
DIR="${MESH_NODE_DIR:-$HOME/.agent-mesh-node}"
START=1
NODE_PID=""
TMP=""

usage() {
	cat <<'EOF'
用法：bash install.sh <网关地址> [选项]

  --ca <路径>              网关 CA 证书文件；提供后用它校验整个流程
  --ca-fingerprint <hex>   网关 CA 的 SHA-256 指纹（DER）；不必手动下载 CA
  --token <邀请码>         邀请码（推荐用环境变量 MESH_TOKEN，避免进入 argv）
  --token-file <路径>      存放邀请码的文件
  --dir <路径>             节点目录（默认 $HOME/.agent-mesh-node）
  --start                  配对成功后立即后台启动节点（默认不启动）
  -h, --help               显示本帮助

  --ca 与 --ca-fingerprint 至少提供一个；同时提供时会两者都校验。
EOF
}

die() {
	printf '错误：%s\n' "$1" >&2
	exit 1
}

cleanup() {
	if [ -n "$TMP" ]; then
		rm -rf "$TMP"
	fi
	return 0
}
trap 'cleanup' EXIT HUP INT TERM

while [ "$#" -gt 0 ]; do
	case "$1" in
	--server)
		SERVER="${2:?}"
		shift 2
		;;
	--ca)
		CA="${2:?}"
		shift 2
		;;
	--ca-fingerprint)
		CA_FP="${2:?}"
		shift 2
		;;
	--token)
		TOKEN="${2:?}"
		shift 2
		;;
	--token-file)
		TOKEN_FILE="${2:?}"
		shift 2
		;;
	--dir)
		DIR="${2:?}"
		shift 2
		;;
	--start)
		START=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	-*)
		echo "未知参数：$1" >&2
		usage >&2
		exit 1
		;;
	*)
		if [ -z "$SERVER" ]; then
			SERVER=$1
		elif [ -z "$TOKEN" ] && [ -z "$TOKEN_FILE" ]; then
			TOKEN_FILE=$1
		else
			DIR=$1
		fi
		shift
		;;
	esac
done

[ -n "$SERVER" ] || {
	usage >&2
	exit 1
}
case "$SERVER" in
https://*) ;;
*) die '必须使用 https 网关地址' ;;
esac
if [ -z "$CA" ] && [ -z "$CA_FP" ]; then
	die '需要 --ca（CA 证书文件）或 --ca-fingerprint（CA 指纹，由控制台给出）'
fi
if [ -n "$CA" ] && [ ! -f "$CA" ]; then
	die "找不到 CA 证书：${CA}"
fi
if [ -z "$TOKEN" ] && [ -z "$TOKEN_FILE" ]; then
	die '缺少邀请码：用 MESH_TOKEN 环境变量或 --token / --token-file 提供'
fi
if [ -n "$TOKEN_FILE" ] && [ ! -f "$TOKEN_FILE" ]; then
	die "找不到邀请码文件：${TOKEN_FILE}"
fi

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
x86_64) ARCH=amd64 ;;
arm64 | aarch64) ARCH=arm64 ;;
*) die "不支持的架构：$ARCH" ;;
esac
case "$OS" in
darwin | linux) ;;
*) die "不支持的系统：$OS" ;;
esac

# curl 与 wget 任一即可；curl 优先（Windows 10+ 也自带 curl.exe）。
if command -v curl >/dev/null 2>&1; then
	fetch_tls() { curl --cacert "$CA" --proto '=https' --tlsv1.2 --fail --silent --show-error --location --max-time 120 --max-filesize 268435456 "$1" -o "$2"; }
	fetch_text_tls() { curl --cacert "$CA" --proto '=https' --tlsv1.2 --fail --silent --show-error --location --max-time 60 --max-filesize 65536 "$1"; }
	fetch_raw() { curl -k --proto '=https' --tlsv1.2 --fail --silent --show-error --location --max-time 120 --max-filesize 268435456 "$1" -o "$2"; }
elif command -v wget >/dev/null 2>&1; then
	fetch_tls() { wget --ca-certificate="$CA" -q -O "$2" "$1"; }
	fetch_text_tls() { wget --ca-certificate="$CA" -q -O - "$1"; }
	fetch_raw() { wget --no-check-certificate -q -O "$2" "$1"; }
else
	die '需要 curl 或 wget 之一'
fi

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

# CA 指纹 = 证书 DER 的 SHA-256，与网关启动时打印的值一致。
# 优先用 openssl 转 DER；没有 openssl 时直接对 PEM 的 base64 正文解码得到 DER。
ca_fingerprint() {
	if command -v openssl >/dev/null 2>&1; then
		openssl x509 -in "$1" -outform der 2>/dev/null >"$TMP/ca.der" || return 1
	else
		sed -n '/BEGIN CERTIFICATE/,/END CERTIFICATE/p' "$1" |
			sed '1d;$d' | tr -d '\n\r' |
			{ base64 -d 2>/dev/null || base64 -D 2>/dev/null; } >"$TMP/ca.der" || return 1
	fi
	[ -s "$TMP/ca.der" ] || return 1
	sha256_of "$TMP/ca.der"
}

lower() { tr 'A-Z' 'a-z'; }

TMP=$(mktemp -d)

# 1. 准备可信的 CA：要么来自本地文件，要么从网关取回并按指纹校验。
if [ -z "$CA" ]; then
	CA="$TMP/ca.crt"
	echo "取回网关 CA：$SERVER/download/ca.crt"
	fetch_raw "$SERVER/download/ca.crt" "$CA" || die '取回网关 CA 失败'
fi
ACTUAL_FP=$(ca_fingerprint "$CA") || die '无法计算 CA 指纹（需要 openssl 或 base64 + sha256sum/shasum）'
if [ -n "$CA_FP" ]; then
	EXPECTED_FP=$(printf '%s' "$CA_FP" | lower)
	[ "$ACTUAL_FP" = "$EXPECTED_FP" ] ||
		die "CA 指纹不匹配：期望 ${EXPECTED_FP}，实际 ${ACTUAL_FP}。请勿继续，可能正在被中间人替换。"
fi

mkdir -p "$DIR"
echo "网关：$SERVER"
echo "节点目录：$DIR"
echo "CA SHA-256：${ACTUAL_FP}（应与控制台显示的一致）"

# 2. 可选的最强路径：独立可信验证器做发行签名验证。
STRONG=0
if [ -n "${MESH_VERIFY_BIN:-}" ] || [ -n "${MESH_RELEASE_KEY:-}" ]; then
	: "${MESH_VERIFY_BIN:?使用强校验时需同时提供 MESH_VERIFY_BIN 与 MESH_RELEASE_KEY}"
	: "${MESH_RELEASE_KEY:?使用强校验时需同时提供 MESH_VERIFY_BIN 与 MESH_RELEASE_KEY}"
	[ -x "$MESH_VERIFY_BIN" ] || die "MESH_VERIFY_BIN 不可执行：${MESH_VERIFY_BIN}"
	[ -r "$MESH_RELEASE_KEY" ] || die "MESH_RELEASE_KEY 不可读：${MESH_RELEASE_KEY}"
	STRONG=1
fi

if [ "$STRONG" = 1 ]; then
	echo '校验等级：完整验签（独立验证器 + 带外公钥）'
	"$MESH_VERIFY_BIN" release fetch --server "$SERVER" --ca "$CA" \
		--public-key "$MESH_RELEASE_KEY" --target "$OS-$ARCH" --out "$DIR/mesh"
	for name in LICENSE NOTICE THIRD_PARTY_NOTICES; do
		"$MESH_VERIFY_BIN" release fetch --server "$SERVER" --ca "$CA" \
			--public-key "$MESH_RELEASE_KEY" --target "$name" --out "$DIR/$name"
	done
else
	# 默认路径：下载经 CA 验证的网关 TLS；CA 要么来自本地文件，要么已按指纹核对。
	echo '校验等级：经 CA 验证的 HTTPS 通道（如需发行签名验证，请设置 MESH_VERIFY_BIN 与 MESH_RELEASE_KEY）'
	fetch_tls "$SERVER/download/mesh?arch=${OS}-${ARCH}" "$DIR/mesh" || die '下载 mesh 失败'
	for name in LICENSE NOTICE THIRD_PARTY_NOTICES; do
		fetch_tls "$SERVER/download/release/$name" "$DIR/$name" 2>/dev/null || rm -f "$DIR/$name"
	done
	if EXPECTED=$(fetch_text_tls "$SERVER/download/mesh.sha256?arch=${OS}-${ARCH}"); then
		EXPECTED=$(printf '%s' "$EXPECTED" | awk '{print $1}')
		if [ -n "$EXPECTED" ]; then
			ACTUAL=$(sha256_of "$DIR/mesh")
			[ "$ACTUAL" = "$EXPECTED" ] || {
				rm -f "$DIR/mesh"
				die "SHA-256 校验失败：期望 ${EXPECTED}，实际 ${ACTUAL}"
			}
		fi
	fi
fi
chmod 755 "$DIR/mesh"

# 准备标准可执行文件 bin 目录
BIN_INSTALL=""
if [ -w "/usr/local/bin" ]; then
	BIN_INSTALL="/usr/local/bin/mesh"
elif [ -d "$HOME/.local/bin" ] || mkdir -p "$HOME/.local/bin" 2>/dev/null; then
	BIN_INSTALL="$HOME/.local/bin/mesh"
elif [ -d "$HOME/bin" ] || mkdir -p "$HOME/bin" 2>/dev/null; then
	BIN_INSTALL="$HOME/bin/mesh"
fi

if [ -n "$BIN_INSTALL" ]; then
	rm -f "$BIN_INSTALL" 2>/dev/null || true
	cp -f "$DIR/mesh" "$BIN_INSTALL" 2>/dev/null || ln -sf "$DIR/mesh" "$BIN_INSTALL" 2>/dev/null || true
	chmod 755 "$BIN_INSTALL" 2>/dev/null || true
fi

# 3. 配对：邀请码经 stdin 传给 mesh，不出现在 URL 或 argv 中。
TOKEN_SOURCE=""
if [ -n "$TOKEN" ]; then
	TOKEN_SOURCE="$DIR/.invitation"
	(umask 077 && printf '%s\n' "$TOKEN" >"$TOKEN_SOURCE")
else
	TOKEN_SOURCE="$TOKEN_FILE"
fi

# 若当前机器已有运行中的节点，先停止旧实例，避免多个进程产生重复设备
if [ -f "$DIR/node.pid" ]; then
	"$DIR/mesh" node stop --dir "$DIR" >/dev/null 2>&1 || true
fi

# 若已有旧配对，自动归档备份，保证重新配对顺畅完成
if [ -f "$DIR/node.key" ]; then
	echo "检测到已有配对信息，正在备份并更新节点身份..."
	BACKUP_DIR="$DIR/backup-$(date +%Y%m%d%H%M%S)"
	mkdir -p "$BACKUP_DIR"
	mv "$DIR"/node.* "$BACKUP_DIR/" 2>/dev/null || true
fi

"$DIR/mesh" pair --server "$SERVER" --ca "$CA" --token - --dir "$DIR" <"$TOKEN_SOURCE"
[ -n "$TOKEN" ] && rm -f "$TOKEN_SOURCE"

if [ "$STRONG" = 1 ] && [ ! -e "$DIR/release.pub" ]; then
	cp -n "$MESH_RELEASE_KEY" "$DIR/release.pub"
fi

echo '安装与配对完成。'
# 如果带有 --start 或默认自动启动守护
if [ "${START:-0}" = 1 ] || [ "${AUTO_START:-1}" = 1 ]; then
	"$DIR/mesh" node start --dir "$DIR"
else
	echo "常用管理命令："
	echo "  mesh node start      # 在后台启动节点"
	echo "  mesh node status     # 查看节点运行状态与最新日志"
	echo "  mesh node stop       # 停止节点进程"
	echo "  mesh node restart    # 重启节点进程"
	echo "  mesh node reload     # 重载节点配置"
	echo "  mesh node            # 前台交互式运行"
fi
