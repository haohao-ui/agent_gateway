<#
Agent Gateway 节点安装脚本（由网关分发）。

一键安装（先从控制台「下载 CA 证书」保存为 .\ca.crt，然后执行）：
  $env:MESH_TOKEN='<邀请码>'; irm https://<网关>:8443/download/install.ps1 -OutFile install.ps1
  ./install.ps1 -Server https://<网关>:8443 -Ca .\ca.crt

也可显式传参：
  ./install.ps1 -Server https://<网关>:8443 -Ca .\ca.crt -TokenFile .\invitation.txt -Dir .\node [-Start]

信任模型：脚本与后续所有下载都用网关 CA 验证 TLS（依赖 Windows 自带的 curl.exe），
信任锚是你手里这份 CA 证书。设置 MESH_VERIFY_BIN 与 MESH_RELEASE_KEY 可额外要求
独立验证器做发行签名验证（最强路径），此时不做任何降级。

邀请码只经环境变量或文件传递，不进入 URL 或命令行参数；配对时通过 stdin 交给 mesh。
#>
param(
    [Parameter(Position = 0)][string]$Server,
    [string]$Ca = $(if ($env:MESH_TLS_CA) { $env:MESH_TLS_CA } elseif ($env:MESH_CA) { $env:MESH_CA } else { '.\ca.crt' }),
    [string]$Token = $env:MESH_TOKEN,
    [string]$TokenFile,
    [string]$Dir = $(if ($env:MESH_NODE_DIR) { $env:MESH_NODE_DIR } else { "$env:USERPROFILE\.agent-mesh-node" }),
    [switch]$Start,
    [switch]$Help
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

if ($Help) {
    @'
用法：./install.ps1 -Server <网关地址> [选项]

  -Ca <路径>          网关 CA 证书（默认 .\ca.crt，也可用 MESH_TLS_CA / MESH_CA）
  -Token <邀请码>     邀请码（推荐用环境变量 MESH_TOKEN）
  -TokenFile <路径>   存放邀请码的文件
  -Dir <路径>         节点目录（默认 $env:USERPROFILE\.agent-mesh-node）
  -Start              配对成功后立即后台启动节点（默认不启动）
'@
    exit 0
}

function Fail([string]$Message) {
    Write-Error "错误：$Message"
    exit 1
}

if (-not $Server) { Fail '需要网关地址，例如 ./install.ps1 -Server https://gateway.example:8443' }
if ($Server -notmatch '^https://') { Fail "必须使用 https 网关地址：$Server" }
if (-not (Test-Path -LiteralPath $Ca -PathType Leaf)) { Fail "找不到 CA 证书：$Ca（请先在控制台点「下载 CA 证书」）" }
if (-not $Token -and -not $TokenFile) { Fail '缺少邀请码：用 MESH_TOKEN 环境变量或 -Token / -TokenFile 提供' }
if ($TokenFile -and -not (Test-Path -LiteralPath $TokenFile -PathType Leaf)) { Fail "找不到邀请码文件：$TokenFile" }

$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$target = "windows-$arch"

# 依赖 Windows 10 1803+ 自带的 curl.exe，与 macOS/Linux 脚本保持同一套 TLS 校验方式。
$curl = Join-Path $env:SystemRoot 'System32\curl.exe'
if (-not (Test-Path -LiteralPath $curl)) {
    $found = Get-Command curl.exe -ErrorAction SilentlyContinue
    if ($found) { $curl = $found.Source } else { Fail '需要 curl.exe（Windows 10 1803+ 自带）；或改用强校验路径并自行准备验证器' }
}

function Fetch([string]$Url, [string]$Destination) {
    & $curl --cacert $Ca --proto '=https' --tlsv1.2 --fail --silent --show-error `
        --location --max-time 120 --max-filesize 268435456 $Url --output $Destination
    if ($LASTEXITCODE -ne 0) { Fail "下载失败：$Url" }
}

$caFingerprint = ''
try {
    $cert = New-Object System.Security.Cryptography.X509Certificates.X509Certificate2((Resolve-Path -LiteralPath $Ca).Path)
    $caFingerprint = ($cert.GetCertHashString('SHA256')).ToLower()
}
catch { $caFingerprint = '' }

New-Item -ItemType Directory -Force -Path $Dir | Out-Null
Write-Host "网关：$Server"
Write-Host "节点目录：$Dir"
if ($caFingerprint) { Write-Host "CA SHA-256：$caFingerprint（应与网关终端或控制台显示的一致）" }

$strong = $false
if ($env:MESH_VERIFY_BIN -or $env:MESH_RELEASE_KEY) {
    if (-not $env:MESH_VERIFY_BIN -or -not $env:MESH_RELEASE_KEY) {
        Fail '使用强校验时需同时提供 MESH_VERIFY_BIN 与 MESH_RELEASE_KEY'
    }
    if (-not (Test-Path -LiteralPath $env:MESH_VERIFY_BIN -PathType Leaf)) { Fail "MESH_VERIFY_BIN 不存在：$($env:MESH_VERIFY_BIN)" }
    if (-not (Test-Path -LiteralPath $env:MESH_RELEASE_KEY -PathType Leaf)) { Fail "MESH_RELEASE_KEY 不存在：$($env:MESH_RELEASE_KEY)" }
    $strong = $true
}

$exe = Join-Path $Dir 'mesh.exe'
if ($strong) {
    Write-Host '校验等级：完整验签（独立验证器 + 带外公钥）'
    & $env:MESH_VERIFY_BIN release fetch --server $Server --ca $Ca --public-key $env:MESH_RELEASE_KEY --target $target --out $exe
    if ($LASTEXITCODE -ne 0) { Fail '发行签名验证失败' }
    foreach ($name in @('LICENSE', 'NOTICE', 'THIRD_PARTY_NOTICES')) {
        & $env:MESH_VERIFY_BIN release fetch --server $Server --ca $Ca --public-key $env:MESH_RELEASE_KEY --target $name --out (Join-Path $Dir $name)
        if ($LASTEXITCODE -ne 0) { Fail '发行声明文件验证失败' }
    }
}
else {
    Write-Host '校验等级：经 CA 验证的 HTTPS 通道（如需发行签名验证，请设置 MESH_VERIFY_BIN 与 MESH_RELEASE_KEY）'
    Fetch "$Server/download/mesh?arch=$target" $exe
    foreach ($name in @('LICENSE', 'NOTICE', 'THIRD_PARTY_NOTICES')) {
        $destination = Join-Path $Dir $name
        try { Fetch "$Server/download/release/$name" $destination }
        catch { Remove-Item -Force $destination -ErrorAction SilentlyContinue }
    }
    $expected = ''
    try {
        $sums = & $curl --cacert $Ca --proto '=https' --tlsv1.2 --silent --show-error `
            --location --max-time 60 --max-filesize 65536 "$Server/download/mesh.sha256?arch=$target"
        if ($LASTEXITCODE -eq 0 -and $sums) { $expected = ($sums -split '\s+')[0] }
    }
    catch { $expected = '' }
    if ($expected) {
        $actual = (Get-FileHash -Algorithm SHA256 -Path $exe).Hash.ToLower()
        if ($actual -ne $expected.ToLower()) {
            Remove-Item -Force $exe -ErrorAction SilentlyContinue
            Fail "SHA-256 校验失败：期望 $expected，实际 $actual"
        }
    }
}

# 配对：邀请码经 stdin 传给 mesh，不出现在 URL 或命令行参数中。
$tokenSource = $TokenFile
if ($Token) {
    $tokenSource = Join-Path $Dir '.invitation'
    Set-Content -LiteralPath $tokenSource -Value $Token -NoNewline -Encoding ascii
}
try {
    Get-Content -Raw -LiteralPath $tokenSource | & $exe pair --server $Server --ca $Ca --token - --dir $Dir
    if ($LASTEXITCODE -ne 0) { Fail '配对失败' }
}
finally {
    if ($Token -and (Test-Path -LiteralPath $tokenSource)) { Remove-Item -Force $tokenSource }
}

if ($strong) {
    $pub = Join-Path $Dir 'release.pub'
    if (-not (Test-Path -LiteralPath $pub)) { Copy-Item -LiteralPath $env:MESH_RELEASE_KEY -Destination $pub }
}

Write-Host '安装与配对完成。'
if ($Start) {
    Start-Process -FilePath $exe -ArgumentList @('node', '--dir', $Dir, '--config', (Join-Path $Dir 'node.json')) `
        -RedirectStandardOutput (Join-Path $Dir 'node.log') -RedirectStandardError (Join-Path $Dir 'node.err.log') -WindowStyle Hidden
    Write-Host "节点已在后台启动，日志：$(Join-Path $Dir 'node.log')"
}
else {
    Write-Host "检查 $(Join-Path $Dir 'node.json') 后手动启动："
    Write-Host "  $exe node --dir `"$Dir`" --config `"$(Join-Path $Dir 'node.json')`""
}
