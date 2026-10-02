# Send an ELF payload to the PS5 ELF loader and print whatever it writes back.
#   $env:PS5_HOST = '192.168.1.50'
#   .\tools\ps5send.ps1 -File out\hello.elf [-Seconds 20]
param(
    [Parameter(Mandatory = $true)][string]$File,
    [string]$PS5Host = $env:PS5_HOST,   # the console's address
    [int]$Port = 9021,
    [int]$Seconds = 20,      # stop listening after this long
    [int]$IdleSeconds = 0    # if > 0, also stop after this long without output
)

if (-not $PS5Host) { throw 'Set $env:PS5_HOST or pass -PS5Host with the console''s address.' }
$bytes = [IO.File]::ReadAllBytes((Resolve-Path $File))
$client = [Net.Sockets.TcpClient]::new()
$client.NoDelay = $true
$client.Connect($PS5Host, $Port)
$stream = $client.GetStream()
$stream.Write($bytes, 0, $bytes.Length)
$stream.Flush()
Write-Host ("[sent {0:N0} bytes to {1}:{2}]" -f $bytes.Length, $PS5Host, $Port)

$buf = [byte[]]::new(65536)
$deadline = [DateTime]::UtcNow.AddSeconds($Seconds)
$lastData = [DateTime]::UtcNow
$closed = $false
while ([DateTime]::UtcNow -lt $deadline) {
    if ($client.Client.Poll(200000, [Net.Sockets.SelectMode]::SelectRead)) {
        $n = 0
        try { $n = $stream.Read($buf, 0, $buf.Length) } catch { $closed = $true; break }
        if ($n -le 0) { $closed = $true; break }
        Write-Host -NoNewline ([Text.Encoding]::UTF8.GetString($buf, 0, $n))
        $lastData = [DateTime]::UtcNow
    } elseif ($IdleSeconds -gt 0 -and ([DateTime]::UtcNow - $lastData).TotalSeconds -gt $IdleSeconds) {
        break
    }
}
Write-Host ''
if ($closed) { Write-Host '[connection closed by PS5]' } else { Write-Host '[stopped listening]' }
$client.Close()
