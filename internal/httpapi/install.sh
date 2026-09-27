#!/usr/bin/env bash
# Agent Gateway 节点安装脚本（由网关分发）。
#
# 一键安装（先从控制台「下载 CA 证书」保存为 ./ca.crt，然后粘贴一行）：
#   curl --cacert ./ca.crt -fsSL https://<网关>:8443/download/install.sh | MESH_TOKEN='<邀请码>' bash -s -- https://<网关>:8443
#
# 也可显式传参：
#   bash install.sh https://<网关>:8443 --ca ./ca.crt --token-file ./invitation.txt --dir ./node [--start]
#
# 信任模型：脚本本身与后续所有下载都用 --cacert 验证网关 TLS，信任锚是你手里这份
# CA 证书。设置 MESH_VERIFY_BIN 与 MESH_RELEASE_KEY 可额外要求独立验证器做发行签名
# 验证（最强路径），此时不做任何降级。
#
# 邀请码只经环境变量或文件传递，不进入 URL 或命令行参数；配对时通过 stdin 交给 mesh。
set -euo pipefail
umask 077

SERVER=""
CA="${MESH_TLS_CA:-${MESH_CA:-./ca.crt}}"
TOKEN="${MESH_TOKEN:-}"
TOKEN_FILE=""
DIR="${MESH_NODE_DIR:-$HOME/.agent-mesh-node}"
START=0

usage() {
	cat <<'EOF'
用法：bash install.sh <网关地址> [选项]

  --ca <路径>         网关 CA 证书（默认 ./ca.crt，也可用 MESH_TLS_CA / MESH_CA）
  --token <邀请码>    邀请码（推荐用环境变量 MESH_TOKEN，避免进入 argv）
  --token-file <路径> 存放邀请码的文件
  --dir <路径>        节点目录（默认 $HOME/.agent-mesh-node）
  --start             配对成功后立即后台启动节点（默认不启动）
  -h, --help          显示本帮助
EOF
}

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
*)
	echo '必须使用 https 网关地址' >&2
	exit 1
	;;
esac
[ -f "$CA" ] || {
	echo "找不到 CA 证书：${CA}（请先在控制台点「下载 CA 证书」，保存为 ./ca.crt）" >&2
	exit 1
}
if [ -z "$TOKEN" ] && [ -z "$TOKEN_FILE" ]; then
	echo '缺少邀请码：用 MESH_TOKEN 环境变量或 --token / --token-file 提供' >&2
	exit 1
fi
if [ -n "$TOKEN_FILE" ] && [ ! -f "$TOKEN_FILE" ]; then
	echo "找不到邀请码文件：${TOKEN_FILE}" >&2
	exit 1
fi

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
x86_64) ARCH=amd64 ;;
arm64 | aarch64) ARCH=arm64 ;;
*)
	echo "不支持的架构：$ARCH" >&2
	exit 1
	;;
esac
case "$OS" in
darwin | linux) ;;
*)
	echo "不支持的系统：$OS" >&2
	exit 1
	;;
esac

# CA 指纹（DER 的 SHA-256），与网关启动时打印的 "CA SHA-256" 一致，便于带外核对。
CA_FP=""
if command -v openssl >/dev/null 2>&1; then
	CA_FP=$(openssl x509 -in "$CA" -outform der 2>/dev/null | { sha256sum 2>/dev/null || shasum -a 256; } | awk '{print $1}') || CA_FP=""
fi

mkdir -p "$DIR"
echo "网关：$SERVER"
echo "节点目录：$DIR"
[ -n "$CA_FP" ] && echo "CA SHA-256：${CA_FP}（应与网关终端或控制台显示的一致）"

STRONG=0
if [ -n "${MESH_VERIFY_BIN:-}" ] || [ -n "${MESH_RELEASE_KEY:-}" ]; then
	: "${MESH_VERIFY_BIN:?使用强校验时需同时提供 MESH_VERIFY_BIN 与 MESH_RELEASE_KEY}"
	: "${MESH_RELEASE_KEY:?使用强校验时需同时提供 MESH_VERIFY_BIN 与 MESH_RELEASE_KEY}"
	[ -x "$MESH_VERIFY_BIN" ] || {
		echo "MESH_VERIFY_BIN 不可执行：${MESH_VERIFY_BIN}" >&2
		exit 1
	}
	[ -r "$MESH_RELEASE_KEY" ] || {
		echo "MESH_RELEASE_KEY 不可读：${MESH_RELEASE_KEY}" >&2
		exit 1
	}
	STRONG=1
fi

if [ "$STRONG" = 1 ]; then
	# 最强路径：独立可信验证器做发行签名验证，逐文件校验后才落盘。
	echo '校验等级：完整验签（独立验证器 + 带外公钥）'
	"$MESH_VERIFY_BIN" release fetch --server "$SERVER" --ca "$CA" \
		--public-key "$MESH_RELEASE_KEY" --target "$OS-$ARCH" --out "$DIR/mesh"
	for name in LICENSE NOTICE THIRD_PARTY_NOTICES; do
		"$MESH_VERIFY_BIN" release fetch --server "$SERVER" --ca "$CA" \
			--public-key "$MESH_RELEASE_KEY" --target "$name" --out "$DIR/$name"
	done
else
	# 默认路径：所有下载都经 --cacert 验证的网关 TLS，信任锚是本机这份 CA 证书。
	# 校验值与二进制来自同一已认证通道，用于发现损坏；网关自身被替换不在本路径防护范围。
	echo '校验等级：经 CA 验证的 HTTPS 通道（如需发行签名验证，请设置 MESH_VERIFY_BIN 与 MESH_RELEASE_KEY）'
	curl --cacert "$CA" --proto '=https' --tlsv1.2 --fail --silent --show-error \
		--location --max-time 120 --max-filesize 268435456 \
		"$SERVER/download/mesh?arch=${OS}-${ARCH}" -o "$DIR/mesh"
	for name in LICENSE NOTICE THIRD_PARTY_NOTICES; do
		curl --cacert "$CA" --proto '=https' --tlsv1.2 --fail --silent --show-error \
			--location --max-time 120 --max-filesize 268435456 \
			"$SERVER/download/release/$name" -o "$DIR/$name" 2>/dev/null || rm -f "$DIR/$name"
	done
	EXPECTED=$(curl --cacert "$CA" --proto '=https' --tlsv1.2 --fail --silent --show-error \
		--location --max-time 60 --max-filesize 65536 \
		"$SERVER/download/mesh.sha256?arch=${OS}-${ARCH}" | awk '{print $1}') || EXPECTED=""
	if [ -n "$EXPECTED" ]; then
		if command -v sha256sum >/dev/null 2>&1; then
			ACTUAL=$(sha256sum "$DIR/mesh" | awk '{print $1}')
		else
			ACTUAL=$(shasum -a 256 "$DIR/mesh" | awk '{print $1}')
		fi
		[ "$ACTUAL" = "$EXPECTED" ] || {
			rm -f "$DIR/mesh"
			echo "SHA-256 校验失败：期望 ${EXPECTED}，实际 ${ACTUAL}" >&2
			exit 1
		}
	fi
fi
chmod 755 "$DIR/mesh"

# 配对：邀请码经 stdin 传给 mesh，不出现在 URL 或 argv 中。
TOKEN_SOURCE=""
if [ -n "$TOKEN" ]; then
	TOKEN_SOURCE="$DIR/.invitation"
	(umask 077 && printf '%s\n' "$TOKEN" >"$TOKEN_SOURCE")
else
	TOKEN_SOURCE="$TOKEN_FILE"
fi
"$DIR/mesh" pair --server "$SERVER" --ca "$CA" --token - --dir "$DIR" <"$TOKEN_SOURCE"
[ -n "$TOKEN" ] && rm -f "$TOKEN_SOURCE"

if [ "$STRONG" = 1 ] && [ ! -e "$DIR/release.pub" ]; then
	cp -n "$MESH_RELEASE_KEY" "$DIR/release.pub"
fi

echo '安装与配对完成。'
if [ "$START" = 1 ]; then
	nohup "$DIR/mesh" node --dir "$DIR" --config "$DIR/node.json" >"$DIR/node.log" 2>&1 &
	echo "节点已在后台启动（PID $!），日志：$DIR/node.log"
else
	echo "检查 ${DIR}/node.json 后手动启动："
	echo "  $DIR/mesh node --dir \"$DIR\" --config \"$DIR/node.json\""
fi
