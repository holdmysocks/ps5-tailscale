# Build the installer payload around an already built daemon payload.
#   .\tools\build-installer.ps1 [-Daemon out\tailscale.elf] [-Send]
param(
    [string]$Daemon = 'out\tailscale.elf',
    [string]$Out = 'out\tailscale-installer.elf',
    [switch]$Send,
    [int]$Seconds = 70
)

$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'env.ps1')

$daemonPath = (Resolve-Path (Join-Path $DevRoot $Daemon)).Path -replace '\\', '/'
$assets = (Join-Path $DevRoot 'installer\assets') -replace '\\', '/'
$outPath = Join-Path $DevRoot $Out

# The app installer library only loads when these come with it, in this
# order (as in the SDK's install_app sample). With libSceAppInstUtil alone
# the payload never starts: the loader leaves it stopped.
Invoke-PS5CC -O2 -Wall "-DDAEMON_ELF=`"$daemonPath`"" "-DASSET_DIR=`"$assets`"" `
    -lSceIpmi -lSceAppInstUtil -lSceUserService -lSceSystemService `
    -o $outPath (Join-Path $DevRoot 'installer\main.c')

Write-Host ("built {0} ({1:N1} MB)" -f $outPath, ((Get-Item $outPath).Length / 1MB))
if ($Send) {
    & (Join-Path $PSScriptRoot 'ps5send.ps1') -File $outPath -Seconds $Seconds
}
