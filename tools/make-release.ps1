# Build the files of a release into out\release-<version>:
#   tailscale.elf       the payload
#   tailscale.elf.sig   its signature, which the status page's "Install" checks
#   SHA256SUMS.txt
#
#   .\tools\make-release.ps1 -Version 1.2.3
#
# Needs the release signing key on this machine (see docs/BUILDING.md).
param(
    [Parameter(Mandatory = $true)][string]$Version
)

$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'env.ps1')

if ($Version -notmatch '^\d+\.\d+\.\d+$') { throw "version must look like 1.2.3, not '$Version'" }

& (Join-Path $PSScriptRoot 'build-payload.ps1') -GoDir tsd -Name tailscale -Version $Version -HomeIcon

$rel = Join-Path $DevRoot "out\release-$Version"
New-Item -ItemType Directory -Force $rel | Out-Null
$elf = Join-Path $rel 'tailscale.elf'
Copy-Item (Join-Path $DevRoot 'out\tailscale.elf') $elf -Force

Push-Location (Join-Path $DevRoot 'tsd')
try {
    go run ./cmd/signrelease sign -version $Version -file $elf
    if ($LASTEXITCODE -ne 0) { throw 'signing failed' }
    go run ./cmd/signrelease verify -file $elf
    if ($LASTEXITCODE -ne 0) { throw 'the signature does not verify' }
    # The payload must carry the public half of the key it was signed with,
    # or consoles running it could never install the release after it.
    $pub = go run ./cmd/signrelease pubkey
    if (-not (Select-String -Path 'selfupdate.go' -SimpleMatch $pub -Quiet)) {
        throw "tsd\selfupdate.go does not have this machine's public key ($pub) as updatePublicKey"
    }
} finally { Pop-Location }

$hash = (Get-FileHash $elf -Algorithm SHA256).Hash.ToLower()
[IO.File]::WriteAllText((Join-Path $rel 'SHA256SUMS.txt'), "$hash  tailscale.elf`n")

Get-ChildItem $rel | Select-Object Name, Length | Format-Table -AutoSize
Write-Host "release files are in $rel"
