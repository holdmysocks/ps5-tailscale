# Build a Go program for the PS5 and wrap it in the launcher payload.
#   .\tools\build-payload.ps1 -GoDir probe-go -Name probe [-DebugLoader] [-Watchdog 120] [-Send]
param(
    [Parameter(Mandatory = $true)][string]$GoDir,
    [Parameter(Mandatory = $true)][string]$Name,
    [string]$Package = '.',
    [string]$Tags = '',
    [string]$Version = '',     # sets main.version in the Go program
    [string]$MaxProcs = '',    # GOMAXPROCS for the Go program (launcher default: 4)
    [string]$GoDebug = '',     # GODEBUG value baked into the launcher
    [int]$Watchdog = 0,        # test builds: kill the process after this many seconds
    [switch]$DebugLoader,      # print loader details and early crash registers
    [switch]$KeepSymbols,
    [switch]$Send,
    [int]$Seconds = 60
)

$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'env.ps1')

$out = Join-Path $DevRoot 'out'
New-Item -ItemType Directory -Force $out | Out-Null
$bin = Join-Path $out "$Name.bin"
$elf = Join-Path $out "$Name.elf"

$ldflags = @()
if (-not $KeepSymbols) { $ldflags += '-s', '-w' }
if ($Version) { $ldflags += "-X main.version=$Version" }
$goArgs = @('build', '-buildmode=pie', '-trimpath', "-ldflags=$($ldflags -join ' ')", '-o', $bin)
if ($Tags) { $goArgs += "-tags=$Tags" }
$goArgs += $Package

Push-Location (Join-Path $DevRoot $GoDir)
try { Invoke-PS5Go @goArgs } finally { Pop-Location }

$img = $bin -replace '\\', '/'
$ccArgs = @('-O2', '-Wall', "-DGO_IMAGE=`"$img`"")
if ($DebugLoader) { $ccArgs += '-DGOLOAD_DEBUG' }
if ($Watchdog -gt 0) { $ccArgs += "-DGOLOAD_WATCHDOG=$Watchdog" }
if ($MaxProcs) { $ccArgs += "-DGO_MAXPROCS=`"$MaxProcs`"" }
if ($GoDebug) { $ccArgs += "-DGO_DEBUG=`"$GoDebug`"" }
$ccArgs += @('-o', $elf, (Join-Path $DevRoot 'launcher\main.c'), (Join-Path $DevRoot 'launcher\goload.c'))
Invoke-PS5CC @ccArgs

Write-Host ("built {0} ({1:N1} MB)" -f $elf, ((Get-Item $elf).Length / 1MB))
if ($Send) {
    & (Join-Path $PSScriptRoot 'ps5send.ps1') -File $elf -Seconds $Seconds
}
