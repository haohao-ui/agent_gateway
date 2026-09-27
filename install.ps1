param(
 [string]$Version = $env:AGENT_GATEWAY_VERSION,
 [string]$Dir = "$HOME\.local\bin",
 [string]$Repo = 'haohao-ui/agent_gateway',
 [string]$BaseUrl = $env:AGENT_GATEWAY_BASE_URL,
 [string]$Verifier = $env:AGENT_GATEWAY_VERIFY_BIN,
 [string]$PublicKey = $env:AGENT_GATEWAY_PUBLIC_KEY
)
$ErrorActionPreference = 'Stop'
if (!$Verifier -or !(Test-Path -LiteralPath $Verifier -PathType Leaf) -or !$PublicKey -or !(Test-Path -LiteralPath $PublicKey -PathType Leaf)) {
 throw 'An independently trusted mesh verifier and release public key are required.'
}
if (!$Version) { $Version = 'latest' }
if ($Version -notmatch '^[A-Za-z0-9._-]+$') { throw 'Invalid version' }
if (!$BaseUrl) {
 $BaseUrl = if ($Version -eq 'latest') { "https://github.com/$Repo/releases/latest/download" } else { "https://github.com/$Repo/releases/download/$Version" }
}
if (([Uri]$BaseUrl).Scheme -ne 'https') { throw 'HTTPS required' }
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$target = "windows-$arch"
$asset = "mesh-$target.exe"
Add-Type -AssemblyName System.Net.Http
function Get-ReleaseAsset([Uri]$Uri, [string]$Destination) {
 $handler = New-Object System.Net.Http.HttpClientHandler
 $handler.AllowAutoRedirect = $false
 $client = New-Object System.Net.Http.HttpClient($handler)
 $client.Timeout = [TimeSpan]::FromSeconds(90)
 $deadline = New-Object System.Threading.CancellationTokenSource
 $deadline.CancelAfter(90000)
 try {
  for ($redirect = 0; $redirect -le 5; $redirect++) {
   if ($Uri.Scheme -ne 'https' -or $Uri.UserInfo) { throw 'Release redirects must use HTTPS without credentials' }
   $response = $client.GetAsync($Uri, [Net.Http.HttpCompletionOption]::ResponseHeadersRead, $deadline.Token).GetAwaiter().GetResult()
   try {
    $status = [int]$response.StatusCode
    if ($status -ge 300 -and $status -lt 400) {
     if (!$response.Headers.Location) { throw 'Missing redirect location' }
     $Uri = [Uri]::new($Uri, $response.Headers.Location)
     continue
    }
    if (!$response.IsSuccessStatusCode) { throw "Release download failed: $status" }
    $input = $response.Content.ReadAsStreamAsync().GetAwaiter().GetResult()
    $output = [IO.File]::Open($Destination, [IO.FileMode]::CreateNew)
    try {
     $buffer = New-Object byte[] 65536
     [long]$total = 0
     while (($count = $input.ReadAsync($buffer,0,$buffer.Length,$deadline.Token).GetAwaiter().GetResult()) -gt 0) {
      $total += $count
      if ($total -gt 268435456) { throw 'Release asset too large' }
      $output.Write($buffer,0,$count)
     }
    } finally { $output.Dispose(); $input.Dispose() }
    return
   } finally { $response.Dispose() }
  }
  throw 'Too many release redirects'
 } finally { $client.Dispose(); $handler.Dispose(); $deadline.Dispose() }
}
$temp = Join-Path ([IO.Path]::GetTempPath()) ([Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $temp | Out-Null
try {
 # The pinned release signature authenticates files even through GitHub's HTTPS CDN redirects.
 foreach ($name in @('manifest.json','manifest.sig',$asset,'LICENSE','NOTICE','THIRD_PARTY_NOTICES')) {
  Get-ReleaseAsset -Uri "$BaseUrl/$name" -Destination (Join-Path $temp $name)
  if ((Get-Item -LiteralPath (Join-Path $temp $name)).Length -gt 268435456) { throw 'Release asset too large' }
 }
 & $Verifier release verify --dir $temp --public-key $PublicKey --target $target
 if ($LASTEXITCODE -ne 0) { throw 'Release verification failed; installation aborted' }
 New-Item -ItemType Directory -Force -Path $Dir | Out-Null
 $exe = Join-Path $Dir 'mesh.exe'
 if (Test-Path -LiteralPath $exe) {
  if ((Get-Item -LiteralPath $exe).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Refusing to replace a link' }
  Copy-Item -LiteralPath $exe -Destination "$exe.bak" -Force
 }
 Copy-Item -LiteralPath (Join-Path $temp $asset) -Destination $exe -Force
 $notices = Join-Path $Dir 'agent-gateway-notices'
 New-Item -ItemType Directory -Force -Path $notices | Out-Null
 foreach ($name in @('LICENSE','NOTICE','THIRD_PARTY_NOTICES')) { Copy-Item -LiteralPath (Join-Path $temp $name) -Destination (Join-Path $notices $name) -Force }
 & $exe --version
 if ($LASTEXITCODE -ne 0) { throw 'Installed binary did not start; previous binary is in mesh.exe.bak' }
 Write-Host "Installed verified mesh at $exe. No services or shell configuration were changed."
} finally {
 Remove-Item -LiteralPath $temp -Recurse -Force
}
