# Building

The build scripts are PowerShell and were written on Windows 11. Nothing is
installed system-wide; everything lives in the repository folder.

## Layout the scripts expect

```
toolchain\llvm\bin\            clang.exe, ld.lld.exe (LLVM 20 or newer; 23.1.2 was used)
toolchain\ps5-payload-sdk\     ps5-payload-sdk release (v0.43 was used)
goroot\                        Go 1.27.1 with patches\go1.27.1-ps5.patch applied
```

These folders are not in the repository.

## Setting up the toolchain

1. **LLVM.** Download `clang+llvm-23.1.2-x86_64-pc-windows-msvc.tar.xz` from
   the [LLVM releases](https://github.com/llvm/llvm-project/releases) and
   extract at least `bin\clang.exe`, `bin\ld.lld.exe`, `bin\lld.exe` and
   `lib\clang\` into `toolchain\llvm`.

   ```powershell
   tar -xf llvm.tar.xz -C toolchain\llvm --strip-components=1 "*/bin/clang.exe" "*/bin/lld.exe" "*/bin/ld.lld.exe" "*/bin/llvm-readelf.exe" "*/lib/clang/*"
   ```

2. **ps5-payload-sdk.** Download `ps5-payload-sdk.zip` from the
   [SDK releases](https://github.com/ps5-payload-dev/sdk/releases) and unpack
   it into `toolchain\`, which creates `toolchain\ps5-payload-sdk`.

3. **Go.** Download `go1.27.1.windows-amd64.zip` from
   [go.dev](https://go.dev/dl/), unpack it, rename the `go` folder to
   `goroot`, apply the patch, and rebuild the go command and the linker (the
   patch changes a package both of them compile in):

   ```powershell
   tar -xf go1.27.1.windows-amd64.zip
   Rename-Item go goroot
   Set-Location goroot
   git -c core.autocrlf=false apply -p1 ..\patches\go1.27.1-ps5.patch
   Copy-Item bin\go.exe bin\go-bootstrap.exe
   $env:GOTOOLCHAIN = 'local'
   .\bin\go-bootstrap.exe install cmd/go cmd/link
   Set-Location ..
   ```

   If Windows Security blocks the build, keep Go's temporary and cache folders
   inside the repository folder (the scripts do: `.gotmp`, `.gocache`) and
   exclude that folder.

## Building the payloads

```powershell
# C launcher + Go program + home screen icon helper -> out\tailscale.elf
.\tools\build-payload.ps1 -GoDir tsd -Name tailscale -Version 0.4.1 -HomeIcon
```

`-HomeIcon` also builds `appicon\` into `out\appicon.elf` and embeds it in
the launcher.

## Sending to the console

```powershell
$env:PS5_HOST = '192.168.1.50'   # your console
.\tools\ps5send.ps1 -File out\tailscale.elf
```

`ps5send.ps1` prints whatever the payload writes back.

## Tests

The daemon's tests run on the build machine:

```powershell
Set-Location tsd
..\goroot\bin\go.exe test .
```

The daemon also runs on Windows for work on the status page. Build `tsd`
without the PS5 settings, set `PS5TS_DATA` to an empty folder and put a
`config.json` with a free `webAddr` in it.

## Probes

`probe-c\` and `probe-go\` are the small payloads used to find out how the
console behaves. They are useful when porting to another firmware.

```powershell
. .\tools\env.ps1
Invoke-PS5CC -O1 -Wall -o out\sysprobe.elf probe-c\sysprobe.c

# Go probe with the debug loader and a watchdog that kills it after 120 s
.\tools\build-payload.ps1 -GoDir probe-go -Name probe -DebugLoader -Watchdog 120 -Send -Seconds 150
```

Read the scheduling section of [TECHNICAL.md](TECHNICAL.md) before writing
new tests: a payload that spins on every core at the default priority freezes
the console.

## Updating Tailscale or Go

- Tailscale: `go get tailscale.com@<version>` in `tsd` with the patched Go,
  then rebuild. A Tailscale release that needs a newer Go needs the patch
  ported to that Go first.
- Go: apply `patches\go1.27.1-ps5.patch` to the new tree and fix what does
  not apply. The patch touches nine files; TECHNICAL.md says why for each.
