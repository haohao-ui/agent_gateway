<#
Agent Gateway 节点安装脚本（由网关分发）。

推荐用法（控制台给出脚本哈希与 CA 指纹，无需手动下载 CA）：
  curl.exe -kfsSL https://<网关>:8443/download/install.ps1 -o install.ps1
  if ((Get-FileHash install.ps1 -Algorithm SHA256).Hash.ToLower() -ne '<脚本哈希>') { throw '哈希不匹配' }
  $env:MESH_TOKEN='<邀请码>'; ./install.ps1 -Server https://<网关>:8443 -CaFingerprint <CA指纹>

也可用控制台「下载 CA 证书」得到的文件，完全不依赖 -k：
  ./install.ps1 -Server https://<网关>:8443 -Ca .\ca.crt

信任模型：
  提供 -Ca 文件时，脚本与所有下载都用该 CA 校验网关 TLS。
  只提供 -CaFingerprint 时，先从网关取回 CA 并比对指纹（DER 的 SHA-256），
  比对通过后再用该 CA 校验后续全部下载。指纹来自控制台页面，是链路的锚。
  设置 MESH_VERIFY_BIN 与 MESH_RELEASE_KEY 可再要求独立验证器做发行签名验证。

邀请码只经环境变量或文件传递，不进入 URL 或命令行参数；配对时通过 stdin 交给 mesh。
#>
param(
    [Parameter(Position = 0)][string]$Server,
    [string]$Ca = $(if ($env:MESH_TLS_CA) { $env:MESH_TLS_CA } elseif ($env:MESH_CA) { $env:MESH_CA } else { '' }),
    [string]$CaFingerprint = $env:MESH_CA_FINGERPRINT,
    [string]$Token = $env:MESH_TOKEN,
    [string]$TokenFile,
    [string]$Dir = $(if ($env:MESH_NODE_DIR) { $env:MESH_NODE_DIR } else { "$env:USERPROFILE\.agent-mesh-node" }),
    [switch]$Start,
    [switch]$Help
)

$defaultServer = ''
$defaultToken = ''
$defaultCaFp = ''

if (-not $Server -and $defaultServer) { $Server = $defaultServer }
if (-not $Token -and $defaultToken) { $Token = $defaultToken }
if (-not $CaFingerprint -and $defaultCaFp) { $CaFingerprint = $defaultCaFp }
if ($defaultServer -and -not $Start) { $Start = $true }

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

if ($Help) {
    @'
用法：./install.ps1 -Server <网关地址> [选项]

  -Ca <路径>               网关 CA 证书文件；提供后用它校验整个流程
  -CaFingerprint <hex>     网关 CA 的 SHA-256 指纹（DER）；不必手动下载 CA
  -Token <邀请码>          邀请码（推荐用环境变量 MESH_TOKEN）
  -TokenFile <路径>        存放邀请码的文件
  -Dir <路径>              节点目录（默认 $env:USERPROFILE\.agent-mesh-node）
  -Start                   配对成功后立即后台启动节点（默认不启动）

  -Ca 与 -CaFingerprint 至少提供一个；同时提供时会两者都校验。
'@
    exit 0
}

function Fail([string]$Message) {
    Write-Error "错误：$Message"
    exit 1
}

if (-not $Server) { Fail '需要网关地址，例如 ./install.ps1 -Server https://gateway.example:8443' }
if ($Server -notmatch '^https://') { Fail "必须使用 https 网关地址：$Server" }
if (-not $Ca -and -not $CaFingerprint) { Fail '需要 -Ca（CA 证书文件）或 -CaFingerprint（CA 指纹，由控制台给出）' }
if ($Ca -and -not (Test-Path -LiteralPath $Ca -PathType Leaf)) { Fail "找不到 CA 证书：$Ca" }
if (-not $Token -and -not $TokenFile) { Fail '缺少邀请码：用 MESH_TOKEN 环境变量或 -Token / -TokenFile 提供' }
if ($TokenFile -and -not (Test-Path -LiteralPath $TokenFile -PathType Leaf)) { Fail "找不到邀请码文件：$TokenFile" }

$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$target = "windows-$arch"

# 依赖 Windows 10 1803+ 自带的 curl.exe，与 macOS/Linux 脚本保持同一套 TLS 校验方式。
$curl = Join-Path $env:SystemRoot 'System32\curl.exe'
if (-not (Test-Path -LiteralPath $curl)) {
    $found = Get-Command curl.exe -ErrorAction SilentlyContinue
    if ($found) { $curl = $found.Source } else { Fail '需要 curl.exe（Windows 10 1803+ 自带）' }
}

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("agent-gateway-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp -Force | Out-Null

function Fetch([string]$Url, [string]$Destination) {
    & $curl --cacert $Ca --proto '=https' --tlsv1.2 --fail --silent --show-error `
        --location --max-time 120 --max-filesize 268435456 $Url --output $Destination
    if ($LASTEXITCODE -ne 0) { Fail "下载失败：$Url" }
}

function Get-CertFingerprint([string]$Path) {
    $cert = New-Object System.Security.Cryptography.X509Certificates.X509Certificate2((Resolve-Path -LiteralPath $Path).Path)
    return $cert.GetCertHashString('SHA256').ToLower()
}

try {
    # 1. 准备可信的 CA：要么来自本地文件，要么从网关取回并按指纹校验。
    if (-not $Ca) {
        $Ca = Join-Path $tmp 'ca.crt'
        Write-Host "取回网关 CA：$Server/download/ca.crt"
        & $curl -k --proto '=https' --tlsv1.2 --fail --silent --show-error --location `
            --max-time 120 --max-filesize 268435456 "$Server/download/ca.crt" --output $Ca
        if ($LASTEXITCODE -ne 0) { Fail '取回网关 CA 失败' }
    }
    $actualFingerprint = Get-CertFingerprint $Ca
    if ($CaFingerprint -and $actualFingerprint -ne $CaFingerprint.ToLower()) {
        Fail "CA 指纹不匹配：期望 $($CaFingerprint.ToLower())，实际 $actualFingerprint。请勿继续，可能正在被中间人替换。"
    }

    New-Item -ItemType Directory -Force -Path $Dir | Out-Null
    Write-Host "网关：$Server"
    Write-Host "节点目录：$Dir"
    Write-Host "CA SHA-256：$actualFingerprint（应与控制台显示的一致）"

    # 2. 可选的最强路径：独立可信验证器做发行签名验证。
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
    # 若有正在运行的 mesh.exe，先停止以避免 Windows 文件锁定阻碍更新
    Get-Process -Name 'mesh' -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
    Start-Sleep -Milliseconds 500

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

    # 若当前机器已有运行中的节点，先停止旧实例，避免多个进程产生重复设备
    $pidFile = Join-Path $Dir 'node.pid'
    if (Test-Path -LiteralPath $pidFile) {
        try { & $exe node stop --dir $Dir } catch { }
    }

    # 若已有旧配对，自动归档备份，保证重新配对顺畅完成
    $oldKey = Join-Path $Dir 'node.key'
    if (Test-Path -LiteralPath $oldKey) {
        Write-Host "检测到已有配对信息，正在备份并更新节点身份..."
        $backupDir = Join-Path $Dir ("backup-" + (Get-Date -Format 'yyyyMMddHHmmss'))
        New-Item -ItemType Directory -Force -Path $backupDir | Out-Null
        Get-ChildItem -Path $Dir -Filter 'node.*' | Move-Item -Destination $backupDir -Force -ErrorAction SilentlyContinue
    }

    # 3. 配对：邀请码经 stdin 传给 mesh，不出现在 URL 或命令行参数中。
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

    # 将节点目录加入当前用户 PATH，使终端可直接执行 mesh 命令
    try {
        $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
        if ($userPath -and ($userPath -split ';' -notcontains $Dir)) {
            [Environment]::SetEnvironmentVariable('Path', "$userPath;$Dir", 'User')
        }
        if ($env:Path -split ';' -notcontains $Dir) {
            $env:Path = "$env:Path;$Dir"
        }
    }
    catch { }

    if ($Start) {
        & $exe node start --dir $Dir
    }
    else {
        Write-Host "常用管理命令："
        Write-Host "  mesh node start      # 在后台启动节点"
        Write-Host "  mesh node status     # 查看节点运行状态与最新日志"
        Write-Host "  mesh node stop       # 停止节点进程"
        Write-Host "  mesh node restart    # 重启节点进程"
        Write-Host "  mesh node reload     # 重载节点配置"
        Write-Host "  mesh node            # 前台交互式运行"
    }
}
finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
