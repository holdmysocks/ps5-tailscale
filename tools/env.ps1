# Dot-source this file to get the PS5 cross-build helpers.
#   . .\tools\env.ps1

$script:DevRoot = Split-Path -Parent $PSScriptRoot
$script:SdkRoot = Join-Path $DevRoot 'toolchain\ps5-payload-sdk'
$script:LlvmBin = Join-Path $DevRoot 'toolchain\llvm\bin'
$script:GoRoot  = Join-Path $DevRoot 'goroot'

$env:PS5_PAYLOAD_SDK = $SdkRoot
if ($env:PATH -notlike "*$LlvmBin*") {
    $env:PATH = "$LlvmBin;$SdkRoot\win;$env:PATH"
}

# Compile and link C sources into a PS5 payload ELF (same flags as the SDK's
# prospero-clang wrapper).
function Invoke-PS5CC {
    # Plain $args on purpose: a param() block would make PowerShell try to bind
    # compiler flags such as -o to common parameters.
    $CcArgs = $args
    # clang >= 20 links the crt objects itself and finds them through this
    # variable, so crt1.o must not be passed explicitly.
    $env:SCE_PROSPERO_SDK_DIR = $SdkRoot
    & "$LlvmBin\clang.exe" `
        --start-no-unused-arguments `
        -target x86_64-sie-ps5 `
        -fvisibility-nodllstorageclass=default `
        -isysroot $SdkRoot `
        -isystem "$SdkRoot\target\include" `
        -L "$SdkRoot\target\lib" `
        -fno-stack-protector -fno-plt -femulated-tls `
        -lc `
        --end-no-unused-arguments `
        @CcArgs `
        --start-no-unused-arguments `
        --sysroot $SdkRoot `
        '-Wl,--hash-style=gnu' `
        -lkernel_web -lSceLibcInternal -lSceNet `
        --end-no-unused-arguments
    if ($LASTEXITCODE -ne 0) { throw "clang failed ($LASTEXITCODE)" }
}

# Run the patched Go toolchain for the PS5 (FreeBSD/amd64 ABI).
function Invoke-PS5Go {
    $GoArgs = $args
    $old = @{ GOROOT = $env:GOROOT; GOOS = $env:GOOS; GOARCH = $env:GOARCH; CGO_ENABLED = $env:CGO_ENABLED
              GOTOOLCHAIN = $env:GOTOOLCHAIN; GOTMPDIR = $env:GOTMPDIR; GOCACHE = $env:GOCACHE; GOFLAGS = $env:GOFLAGS }
    try {
        $env:GOROOT = $GoRoot
        $env:GOOS = 'freebsd'
        $env:GOARCH = 'amd64'
        $env:CGO_ENABLED = '0'
        $env:GOTOOLCHAIN = 'local'
        $env:GOTMPDIR = Join-Path $DevRoot '.gotmp'
        $env:GOCACHE = Join-Path $DevRoot '.gocache'
        New-Item -ItemType Directory -Force $env:GOTMPDIR, $env:GOCACHE | Out-Null
        & "$GoRoot\bin\go.exe" @GoArgs
        if ($LASTEXITCODE -ne 0) { throw "go failed ($LASTEXITCODE)" }
    } finally {
        foreach ($k in $old.Keys) { Set-Item "env:$k" $old[$k] }
    }
}
