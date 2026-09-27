<#
Agent Gateway 一键安装脚本（Windows）

从 GitHub Release 下载 mesh-windows-<arch>.exe，校验 SHA-256 后安装到本机。
默认安装到 $HOME\.local\bin，不修改 PATH，也不注册后台服务。

用法：
  irm https://github.com/haohao-ui/agent_gateway/releases/latest/download/install.ps1 | iex
  # 指定版本与目录：
  & ([scriptblock]::Create((irm .../install.ps1))) -Version v0.1.1 -Dir "$HOME\bin"

可选环境变量：
  AGENT_GATEWAY_REPO         仓库，默认 haohao-ui/agent_gateway
  AGENT_GATEWAY_VERSION      版本 tag，默认最新正式版
  AGENT_GATEWAY_INSTALL_DIR  安装目录，默认 $HOME\.local\bin
  AGENT_GATEWAY_BASE_URL     直接指定下载基址（镜像站或本地演练用）
#>

[CmdletBinding()]
param(
    [string]$Version = $env:AGENT_GATEWAY_VERSION,
    [string]$Dir = $(if ($env:AGENT_GATEWAY_INSTALL_DIR) { $env:AGENT_GATEWAY_INSTALL_DIR } else { Join-Path $HOME ".local\bin" }),
    [string]$Repo = $(if ($env:AGENT_GATEWAY_REPO) { $env:AGENT_GATEWAY_REPO } else { "haohao-ui/agent_gateway" }),
    [string]$BaseUrl = $env:AGENT_GATEWAY_BASE_URL,
    [switch]$SkipVerify
)

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"

function Fail([string]$Message) {
    Write-Error "错误：$Message"
    exit 1
}

# 1. 探测架构
switch ($env:PROCESSOR_ARCHITECTURE) {
    "AMD64" { $arch = "amd64" }
    "ARM64" { $arch = "arm64" }
    default { Fail "不支持的架构：$($env:PROCESSOR_ARCHITECTURE)" }
}
$asset = "mesh-windows-$arch.exe"

# 2. 确定下载地址
if ($BaseUrl) {
    $base = $BaseUrl.TrimEnd("/")
}
elseif ($Version) {
    if (-not $Version.StartsWith("v")) { $Version = "v$Version" }
    $base = "https://github.com/$Repo/releases/download/$Version"
}
else {
    $base = "https://github.com/$Repo/releases/latest/download"
}

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("agent-gateway-" + [guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $tmp -Force | Out-Null

try {
    Write-Host "平台：windows/$arch"
    Write-Host "下载：$base/$asset"
    $binary = Join-Path $tmp $asset
    try {
        Invoke-WebRequest -Uri "$base/$asset" -OutFile $binary -UseBasicParsing
    }
    catch {
        Fail "下载失败：${base}/${asset}（请确认该版本已发布且平台受支持）"
    }

    # 3. 校验完整性
    if ($SkipVerify) {
        Write-Warning "已按 -SkipVerify 跳过 SHA-256 校验"
    }
    else {
        $sums = Join-Path $tmp "SHA256SUMS"
        try {
            Invoke-WebRequest -Uri "$base/SHA256SUMS" -OutFile $sums -UseBasicParsing
        }
        catch {
            Fail "无法获取 SHA256SUMS，已终止；如确需跳过请加 -SkipVerify"
        }
        # 兼容不同 sha256sum 实现的输出：文件名可能带 "./" 前缀或二进制模式的 "*" 前缀。
        $expected = $null
        foreach ($entry in Get-Content $sums) {
            $parts = $entry -split "\s+"
            if ($parts.Count -lt 2) { continue }
            $name = $parts[1].TrimStart("*") -replace "^\./", ""
            if ($name -eq $asset) { $expected = $parts[0].ToLower(); break }
        }
        if (-not $expected) { Fail "SHA256SUMS 中找不到 $asset" }
        $actual = (Get-FileHash -Algorithm SHA256 -Path $binary).Hash.ToLower()
        if ($expected -ne $actual) {
            Fail "SHA-256 校验失败：期望 ${expected}，实际 ${actual}"
        }
        Write-Host "校验：SHA-256 $actual"
    }

    # 4. 安装
    New-Item -ItemType Directory -Path $Dir -Force | Out-Null
    $target = Join-Path $Dir "mesh.exe"
    if (Test-Path $target) {
        Copy-Item $target "$target.bak" -Force
        Write-Host "已备份原文件到 $target.bak"
    }
    Copy-Item $binary $target -Force

    Write-Host ""
    Write-Host "已安装：$target"
    & $target --version

    # 5. 后续提示
    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    $inPath = ($env:Path -split ";") -contains $Dir
    if (-not $inPath) {
        Write-Host ""
        Write-Host "提示：$Dir 不在 PATH 中，执行以下命令加入当前用户 PATH（重新打开终端后生效）："
        Write-Host "  [Environment]::SetEnvironmentVariable('Path', '$Dir;' + [Environment]::GetEnvironmentVariable('Path','User'), 'User')"
    }

    Write-Host ""
    Write-Host "下一步："
    Write-Host "  mesh server                          启动网关（首次运行自动生成 CA、证书与数据库）"
    Write-Host "  mesh server --help                   查看监听地址、数据目录等参数"
    Write-Host "  mesh credential issue --out op.token 签发操作员令牌"
    Write-Host ""
    Write-Host "注意：Windows 未支持用户级后台服务，自启动请使用任务计划程序。"
    Write-Host ""
    Write-Host "文档："
    Write-Host "  https://github.com/$Repo/blob/main/docs/INSTALL.md"
    Write-Host "  https://github.com/$Repo/blob/main/docs/USAGE.md"
}
finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
