param(
  [Parameter(Mandatory=$true)][string]$ServerHttp,
  [Parameter(Mandatory=$true)][string]$ServerWs,
  [string]$CameraDevice = "Integrated Camera"
)

$cfgDir = Join-Path $env:APPDATA "HomeGuard"
$outboxDir = Join-Path $env:LOCALAPPDATA "HomeGuard\outbox"
New-Item -ItemType Directory -Force -Path $cfgDir | Out-Null
New-Item -ItemType Directory -Force -Path $outboxDir | Out-Null

$cfg = @{
  server_http = $ServerHttp
  server_ws = $ServerWs
  device_id = ""
  device_secret = ""
  device_name = $env:COMPUTERNAME
  outbox_dir = $outboxDir
  camera_device = $CameraDevice
  max_storage_bytes = 5368709120
  poll_interval_seconds = 10
} | ConvertTo-Json

$cfgPath = Join-Path $cfgDir "config.json"
Set-Content -Path $cfgPath -Encoding UTF8 -Value $cfg

Write-Host "Config created: $cfgPath"
Write-Host "Run client with: `$env:HG_CONFIG='$cfgPath'; .\homeguard-client.exe"
