#!/bin/sh
# Agent Gateway 一键安装脚本（macOS / Linux）
#
# 从 GitHub Release 下载对应平台的 mesh 二进制，校验 SHA-256 后安装到本机。
# 默认安装到 ~/.local/bin，不修改 shell 配置，也不注册后台服务。
#
# 用法：
#   curl -fsSL https://github.com/haohao-ui/agent_gateway/releases/latest/download/install.sh | sh
#   curl -fsSL .../install.sh | sh -s -- --version v0.1.1
#   curl -fsSL .../install.sh | sh -s -- --dir "$HOME/bin"
#
# 可选环境变量：
#   AGENT_GATEWAY_REPO         仓库，默认 haohao-ui/agent_gateway
#   AGENT_GATEWAY_VERSION      版本 tag，默认最新正式版
#   AGENT_GATEWAY_INSTALL_DIR  安装目录，默认 $HOME/.local/bin
#   AGENT_GATEWAY_BASE_URL     直接指定下载基址（镜像站或本地演练用）
#   AGENT_GATEWAY_SKIP_VERIFY  设为 1 跳过 SHA-256 校验（不建议）

set -eu

REPO="${AGENT_GATEWAY_REPO:-haohao-ui/agent_gateway}"
VERSION="${AGENT_GATEWAY_VERSION:-}"
INSTALL_DIR="${AGENT_GATEWAY_INSTALL_DIR:-$HOME/.local/bin}"
SKIP_VERIFY="${AGENT_GATEWAY_SKIP_VERIFY:-0}"

err() {
	printf '错误：%s\n' "$1" >&2
	exit 1
}

usage() {
	cat <<'EOF'
用法：install.sh [选项]

  --version <vX.Y.Z>   指定版本（默认最新正式版）
  --dir <路径>         安装目录（默认 ~/.local/bin）
  --repo <owner/repo>  指定仓库（默认 haohao-ui/agent_gateway）
  --skip-verify        跳过 SHA-256 校验（不建议）
  -h, --help           显示本帮助
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--version)
		VERSION="${2:-}"
		[ -n "$VERSION" ] || err "--version 需要一个值"
		shift 2
		;;
	--dir)
		INSTALL_DIR="${2:-}"
		[ -n "$INSTALL_DIR" ] || err "--dir 需要一个值"
		shift 2
		;;
	--repo)
		REPO="${2:-}"
		[ -n "$REPO" ] || err "--repo 需要一个值"
		shift 2
		;;
	--skip-verify)
		SKIP_VERIFY=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		usage >&2
		err "未知选项：$1"
		;;
	esac
done

# 1. 探测平台
os="$(uname -s)"
case "$os" in
Darwin) os=darwin ;;
Linux) os=linux ;;
*) err "不支持的系统：${os}（Windows 请使用 install.ps1）" ;;
esac

machine="$(uname -m)"
case "$machine" in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) err "不支持的架构：$machine" ;;
esac

asset="mesh-${os}-${arch}"

# 2. 选择下载器
if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL "$1" -o "$2"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -qO "$2" "$1"; }
else
	err "需要 curl 或 wget 之一"
fi

# 3. 确定下载地址
if [ -n "${AGENT_GATEWAY_BASE_URL:-}" ]; then
	base="${AGENT_GATEWAY_BASE_URL%/}"
elif [ -n "$VERSION" ]; then
	case "$VERSION" in
	v*) ;;
	*) VERSION="v$VERSION" ;;
	esac
	base="https://github.com/${REPO}/releases/download/${VERSION}"
else
	base="https://github.com/${REPO}/releases/latest/download"
fi

tmp="$(mktemp -d 2>/dev/null || mktemp -d -t agent-gateway)"
trap 'rm -rf "$tmp"' EXIT INT TERM

printf '平台：%s/%s\n' "$os" "$arch"
printf '下载：%s/%s\n' "$base" "$asset"
fetch "$base/$asset" "$tmp/$asset" ||
	err "下载失败：${base}/${asset}（请确认该版本已发布且平台受支持）"

# 4. 校验完整性
if [ "$SKIP_VERIFY" = "1" ]; then
	printf '警告：已按 --skip-verify 跳过 SHA-256 校验\n' >&2
else
	fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS" ||
		err "无法获取 SHA256SUMS，已终止；如确需跳过请加 --skip-verify"
	# 兼容不同 sha256sum 实现的输出：文件名可能带 "./" 前缀或二进制模式的 "*" 前缀。
	expected="$(awk -v a="$asset" '
		{ f = $2; sub(/^\*/, "", f); sub(/^\.\//, "", f); if (f == a) { print $1; exit } }
	' "$tmp/SHA256SUMS")"
	[ -n "$expected" ] || err "SHA256SUMS 中找不到 $asset"
	if command -v sha256sum >/dev/null 2>&1; then
		actual="$(sha256sum "$tmp/$asset" | awk '{print $1}')"
	elif command -v shasum >/dev/null 2>&1; then
		actual="$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')"
	else
		err "缺少 sha256sum 或 shasum，无法校验完整性；如确需跳过请加 --skip-verify"
	fi
	[ "$expected" = "$actual" ] || err "SHA-256 校验失败：期望 ${expected}，实际 ${actual}"
	printf '校验：SHA-256 %s\n' "$actual"
fi

# 5. 安装
mkdir -p "$INSTALL_DIR"
target="$INSTALL_DIR/mesh"
if [ -e "$target" ]; then
	cp "$target" "$target.bak"
	printf '已备份原文件到 %s\n' "$target.bak"
fi
cp "$tmp/$asset" "$target"
chmod 0755 "$target"

printf '\n已安装：%s\n' "$target"
"$target" --version || true

# 6. 后续提示
case ":${PATH}:" in
*":${INSTALL_DIR}:"*) ;;
*)
	printf '\n提示：%s 不在 PATH 中，请加入 shell 配置，例如：\n' "$INSTALL_DIR"
	printf "  echo 'export PATH=\"%s:\$PATH\"' >> ~/.profile\n" "$INSTALL_DIR"
	;;
esac

cat <<EOF

下一步：
  mesh server                          启动网关（首次运行自动生成 CA、证书与数据库）
  mesh server --help                   查看监听地址、数据目录等参数
  mesh credential issue --out op.token 签发操作员令牌
  mesh service install --role server   注册为后台服务（macOS launchd / Linux systemd）

文档：
  https://github.com/${REPO}/blob/main/docs/INSTALL.md
  https://github.com/${REPO}/blob/main/docs/USAGE.md
EOF
