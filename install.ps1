<#
Agent Gateway 安装脚本（Windows）。

一键安装（使用发布页附带的脚本，内嵌该版本各平台二进制的 SHA-256）：
  irm https://github.com/haohao-ui/agent_gateway/releases/latest/download/install.ps1 | iex

强校验（自行提供独立可信的 mesh 验证器与带外公钥）：
  ./install.ps1 -Verifier C:\trusted\mesh.exe -PublicKey C:\trusted\release.pub

信任模型与 install.sh 相同：一键路径锚定 HTTPS 传输 + 脚本内嵌的 SHA-256 指纹
（指纹取自该版本的已签名清单）。本机存在支持 Ed25519 的 openssl 时，会额外验证
清单签名并报告校验等级。不提供跳过校验的选项。
#>

param(
    [string]$Version = $env:AGENT_GATEWAY_VERSION,
    [string]$Dir = $(if ($env:AGENT_GATEWAY_INSTALL_DIR) { $env:AGENT_GATEWAY_INSTALL_DIR } else { "$HOME\.local\bin" }),
    [string]$Repo = $(if ($env:AGENT_GATEWAY_REPO) { $env:AGENT_GATEWAY_REPO } else { 'haohao-ui/agent_gateway' }),
    [string]$BaseUrl = $env:AGENT_GATEWAY_BASE_URL,
    [string]$Verifier = $env:AGENT_GATEWAY_VERIFY_BIN,
    [string]$PublicKey = $env:AGENT_GATEWAY_PUBLIC_KEY,
    [string]$Fingerprint = $env:AGENT_GATEWAY_FINGERPRINT,
    [switch]$Help
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

# >>> PINNED（发版时由 scripts/pin-installers.py 注入；仓库副本保持为空）
$PinnedVersion = ''
$PinnedPublicKey = ''
$PinnedHashes = @{
    'windows-amd64' = ''
    'windows-arm64' = ''
}
# <<< PINNED

function Fail([string]$Message) {
    Write-Error "错误：$Message"
    exit 1
}

if ($Help) {
    @'
用法：./install.ps1 [选项]

一键安装：不带选项即可，使用脚本内嵌的版本指纹。

强校验：
  -Verifier <路径>      独立可信的 mesh.exe 验证器
  -PublicKey <路径>     带外获取的发行公钥

其它：
  -Fingerprint <sha256> 要求公钥指纹必须匹配
  -Version <vX.Y.Z>     指定版本（默认最新正式版）
  -Dir <路径>           安装目录（默认 $HOME\.local\bin）
  -BaseUrl <URL>        指定下载基址（镜像站；必须为 https）
'@
    exit 0
}

if (-not $Version) { $Version = 'latest' }
if ($Version -notmatch '^[A-Za-z0-9._-]+$') { Fail "版本号非法：$Version" }

# 1. 平台探测
switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { $arch = 'amd64' }
    'ARM64' { $arch = 'arm64' }
    default { Fail "不支持的架构：$($env:PROCESSOR_ARCHITECTURE)" }
}
$target = "windows-$arch"
$asset = "mesh-$target.exe"
$pinnedHash = if ($PinnedHashes.ContainsKey($target)) { $PinnedHashes[$target] } else { '' }

# 2. 模式判定
$mode = 'pinned'
if ($Verifier -or $PublicKey) {
    if (-not $Verifier -or -not (Test-Path -LiteralPath $Verifier -PathType Leaf)) { Fail "找不到验证器：$Verifier" }
    if (-not $PublicKey -or -not (Test-Path -LiteralPath $PublicKey -PathType Leaf)) { Fail "找不到发行公钥：$PublicKey" }
    $mode = 'strong'
}
elseif (-not $pinnedHash) {
    Fail "此副本没有内嵌 $target 的指纹。请使用发布页附带的 install.ps1，或用 -Verifier 与 -PublicKey 指定独立可信的验证器与公钥。"
}

# 3. 下载地址
if (-not $BaseUrl) {
    $BaseUrl = if ($Version -eq 'latest') { "https://github.com/$Repo/releases/latest/download" } else { "https://github.com/$Repo/releases/download/$Version" }
}
if ($BaseUrl -notmatch '^https://') { Fail "下载基址必须是 https：$BaseUrl" }
$BaseUrl = $BaseUrl.TrimEnd('/')

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("agent-gateway-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp -Force | Out-Null

function Get-Asset([string]$Name) {
    $destination = Join-Path $tmp $Name
    try {
        Invoke-WebRequest -Uri "$BaseUrl/$Name" -OutFile $destination -UseBasicParsing
    }
    catch {
        Fail "下载失败：$BaseUrl/$Name"
    }
    return $destination
}

function Get-Sha256([string]$Path) {
    return (Get-FileHash -Algorithm SHA256 -Path $Path).Hash.ToLower()
}

try {
    Write-Host "平台：$target"
    Write-Host "来源：$BaseUrl"

    # 4. 下载
    $binary = Get-Asset $asset
    foreach ($notice in @('LICENSE', 'NOTICE', 'THIRD_PARTY_NOTICES')) { Get-Asset $notice | Out-Null }

    # 5. 校验
    $actual = Get-Sha256 $binary
    if ($mode -eq 'strong') {
        Get-Asset 'manifest.json' | Out-Null
        Get-Asset 'manifest.sig' | Out-Null
        & $Verifier release verify --dir $tmp --public-key $PublicKey --target $target
        if ($LASTEXITCODE -ne 0) { Fail '独立验证器拒绝该发行包' }
        $level = "完整验签（独立验证器 $Verifier）"
    }
    else {
        if ($actual -ne $pinnedHash) { Fail "SHA-256 校验失败：期望 $pinnedHash，实际 $actual" }
        $level = '内嵌 SHA-256 指纹（来源：该版本的已签名清单，锚定 HTTPS 传输）'
        $keyFile = $null
        if ($PublicKey) { $keyFile = $PublicKey }
        elseif ($PinnedPublicKey) {
            $keyFile = Join-Path $tmp 'pinned.pub'
            Set-Content -LiteralPath $keyFile -Value $PinnedPublicKey -NoNewline
        }
        if ($Fingerprint) {
            if (-not $keyFile) { Fail '-Fingerprint 需要可用的公钥（发布页脚本或 -PublicKey）' }
            $keyFp = Get-Sha256 $keyFile
            if ($keyFp -ne $Fingerprint.ToLower()) { Fail "公钥指纹不匹配：期望 $Fingerprint，实际 $keyFp" }
            $level = "$level；公钥指纹已按 -Fingerprint 固定"
        }
        # Windows 无内置 Ed25519：仅在存在支持 -rawin 的 openssl 时额外验签。
        if ($keyFile) {
            $openssl = Get-Command openssl -ErrorAction SilentlyContinue
            if ($openssl) {
                $probe = & openssl pkeyutl -help 2>&1 | Out-String
                if ($probe -match '-rawin') {
                    Get-Asset 'manifest.json' | Out-Null
                    Get-Asset 'manifest.sig' | Out-Null
                    & openssl pkeyutl -verify -pubin -inkey $keyFile -rawin -in (Join-Path $tmp 'manifest.json') -sigfile (Join-Path $tmp 'manifest.sig') 2>&1 | Out-Null
                    if ($LASTEXITCODE -ne 0) { Fail '清单签名验证失败：公钥与下载到的清单或签名不一致' }
                    $level = "$level；清单签名验证通过（openssl）"
                }
            }
        }
    }

    # 6. 安装（校验通过后才落盘）
    New-Item -ItemType Directory -Path $Dir -Force | Out-Null
    $noticesDir = Join-Path $Dir 'agent-gateway-notices'
    New-Item -ItemType Directory -Path $noticesDir -Force | Out-Null
    $meshExe = Join-Path $Dir 'mesh.exe'
    if (Test-Path $meshExe) { Copy-Item $meshExe "$meshExe.bak" -Force }
    Copy-Item $binary $meshExe -Force
    foreach ($notice in @('LICENSE', 'NOTICE', 'THIRD_PARTY_NOTICES')) {
        Copy-Item (Join-Path $tmp $notice) (Join-Path $noticesDir $notice) -Force
    }

    Write-Host ''
    Write-Host "校验等级：$level"
    Write-Host "SHA-256：$actual"
    Write-Host "已安装：$meshExe"
    & $meshExe --version

    if (($env:Path -split ';') -notcontains $Dir) {
        Write-Host ''
        Write-Host "提示：$Dir 不在 PATH 中，执行以下命令加入当前用户 PATH（重开终端生效）："
        Write-Host "  [Environment]::SetEnvironmentVariable('Path', '$Dir;' + [Environment]::GetEnvironmentVariable('Path','User'), 'User')"
    }

    $installUrl = if ($Version -eq 'latest') { "https://github.com/$Repo/releases/latest/download/install.ps1" } else { "https://github.com/$Repo/releases/download/$Version/install.ps1" }
    $publishedFp = '（本副本未内嵌公钥）'
    if ($keyFile -and (Test-Path -LiteralPath $keyFile)) { $publishedFp = Get-Sha256 $keyFile }

    Write-Host ''
    Write-Host '下一步：'
    Write-Host '  mesh server                          启动网关（首次运行自动生成 CA、证书与数据库）'
    Write-Host '  mesh credential issue --out op.token 签发操作员令牌'
    Write-Host ''
    Write-Host '注意：Windows 未支持用户级后台服务，自启动请使用任务计划程序。'
    Write-Host ''
    Write-Host '需要不依赖下载服务器的完整验签保证时，把 <公钥文件> 换成你自己的 release.pub：'
    Write-Host "  curl.exe -fsSL $installUrl -o install.ps1"
    Write-Host '  ./install.ps1 -PublicKey <公钥文件>'
    Write-Host ''
    Write-Host '本次脚本内嵌公钥指纹（可用 -Fingerprint 固定核对）：'
    Write-Host "  $publishedFp"
    Write-Host ''
    Write-Host "文档：https://github.com/$Repo/blob/main/docs/INSTALL.md"
}
finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
