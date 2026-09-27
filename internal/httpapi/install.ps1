param(
 [Parameter(Mandatory=$true)][string]$Server,
 [Parameter(Mandatory=$true)][string]$TokenFile,
 [string]$Dir = "$env:USERPROFILE\.agent-mesh-node"
)
$ErrorActionPreference = 'Stop'
if (!$env:MESH_VERIFY_BIN -or !$env:MESH_RELEASE_KEY -or !$env:MESH_TLS_CA) {
 throw 'Set MESH_VERIFY_BIN (trusted executable), MESH_RELEASE_KEY (pinned public key) and MESH_TLS_CA (trusted gateway CA).'
}
if (([Uri]$Server).Scheme -ne 'https') { throw 'HTTPS required' }
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
New-Item -ItemType Directory -Force -Path $Dir | Out-Null
$exe = Join-Path $Dir 'mesh.exe'
& $env:MESH_VERIFY_BIN release fetch --server $Server --ca $env:MESH_TLS_CA --public-key $env:MESH_RELEASE_KEY --target "windows-$arch" --out $exe
if ($LASTEXITCODE -ne 0) { throw 'Release verification failed' }
foreach ($name in @('LICENSE','NOTICE','THIRD_PARTY_NOTICES')) {
 & $env:MESH_VERIFY_BIN release fetch --server $Server --ca $env:MESH_TLS_CA --public-key $env:MESH_RELEASE_KEY --target $name --out (Join-Path $Dir $name)
 if ($LASTEXITCODE -ne 0) { throw 'Release notice verification failed' }
}
Get-Content -Raw -LiteralPath $TokenFile | & $exe pair --server $Server --ca $env:MESH_TLS_CA --token - --dir $Dir
if ($LASTEXITCODE -ne 0) { throw 'Pairing failed' }
$pub = Join-Path $Dir 'release.pub'
if (Test-Path -LiteralPath $pub) { throw 'Release trust key already exists; review it locally' }
Copy-Item -LiteralPath $env:MESH_RELEASE_KEY -Destination $pub
Write-Host 'Verified installation and pairing complete. Review node.json before starting the node.'
