# Tailscale for jailbroken PS5

Puts a jailbroken PS5 on your [Tailscale](https://tailscale.com) network.

- **Reach the console from anywhere.** FTP, the payload loader, web tools:
  whatever listens on the console is available at its tailnet address from
  your other Tailscale devices.
- **Remote Play over Tailscale.** Play the PS5 from anywhere with a Remote
  Play client, at the console's tailnet address, with no port forwarding on
  your router.
- **Stream games to the console over Tailscale.** A Moonlight client on the
  PS5 (such as ProsperoLight) can connect to a Sunshine host on your tailnet.
- **Home screen icon** that opens its status page.

It runs the real Tailscale client code (v1.104.0) as a single payload.

![The status page](docs/status-page.png)

> **Unofficial and experimental.** This project is not affiliated with or
> endorsed by Tailscale Inc. or Sony. It has been tested on one console. It
> runs homebrew with full privileges on a jailbroken system; use it at your
> own risk.

## What it is not

The PS5 kernel has no tunnel device, so Tailscale cannot become a system-wide
VPN here. It runs inside one process:

- Games and PSN traffic do **not** go through Tailscale.
- Other apps on the console cannot open connections to tailnet addresses
  directly. They can through a *local forward* (see
  [game streaming](#game-streaming-moonlight-to-sunshine) and
  [configuration](#configuration)).
- No exit node, subnet routing, Tailscale SSH, Taildrop or Funnel.

## Requirements

- A jailbroken PS5 with an ELF loader listening on port 9021
  (for example [elfldr](https://github.com/ps5-payload-dev/elfldr)).
- A Tailscale account.

Tested on firmware 13.42 with elfldr 0.26.

## Install

1. Download `tailscale-installer.elf` from the
   [latest release](../../releases/latest).
2. Send it to the console's ELF loader, once. Any payload sender works:

   ```bash
   # Linux / macOS
   socat -t 60 - TCP:<console-ip>:9021 < tailscale-installer.elf
   ```

   ```powershell
   # Windows (script from this repository)
   .\tools\ps5send.ps1 -File tailscale-installer.elf -PS5Host <console-ip> -Seconds 70
   ```

   The installer prints what it does. It:
   - stores the daemon payload as `/data/tailscale/tailscale.elf`,
   - adds a **Tailscale** icon to the home screen (media section),
   - starts Tailscale.

3. Open `http://<console-ip>:8090` on a phone or PC. Scan the QR code or
   follow the link and log in to Tailscale. If your tailnet uses device
   approval, approve the console in the admin console.

The console now has a tailnet address, shown on the status page.

Sending the installer again upgrades and restarts Tailscale. The login is
kept.

Tailscale runs until the console restarts. After a restart and jailbreak,
send `/data/tailscale/tailscale.elf` (or the installer) to the ELF loader
again. The installer does not change any payload autoloader; if you use one,
you can add that file to it yourself.

## Using it

### Reaching the console

Connect to the console's tailnet address or MagicDNS name on the port you
want, for example FTP on 2121 or the payload loader on 9021.

- Every TCP port that something on the console listens on is forwarded.
  Ports with no listener refuse the connection.
- UDP ports have to be listed, in `udpPorts` in the
  [configuration](#configuration). The default list is Remote Play's.
- To keep a TCP port off the tailnet, add it to `blockedPorts`.

### Remote Play

The console's own Remote Play service is reachable at its tailnet address, so
a Remote Play client on any of your Tailscale devices can connect from
anywhere. Any client that lets you enter the console's address works.

1. On the console, enable Remote Play (Settings > System > Remote Play).
2. Register your Remote Play client with the console as usual. This is
   easiest at home on the same network; see the client's documentation.
3. In the client, add the console manually with its **tailnet address**
   (shown on the status page).
4. Connect.

Tested and working with Chiaki, and with Asobi on iOS and Android.

How it works: Remote Play uses TCP 9295 and UDP 9295, 9296, 9297 and 9302.
The TCP port is forwarded like any other; the daemon listens on the UDP
ports on the console's tailnet addresses and relays them to the service.

Notes:

- The video passes through the daemon, which runs at the lowest priority so
  that it never takes time from a game. Under a demanding game that may show
  as stutter.
- Waking the console from rest mode does not work: nothing is running then.

### The status page

`http://<console address>:8090`, on the LAN or over the tailnet, or the
**Tailscale** icon on the home screen. It shows the connection state, the
login link, and your devices, and has controls for game streaming, logging
out, stopping and uninstalling.

It has **no password**, like the console's other homebrew services. Anyone on
your LAN, or on your tailnet if your ACLs allow it, can use it.

### Game streaming (Moonlight to Sunshine)

This lets a Moonlight client on the PS5 stream from a PC that is somewhere
else, over Tailscale.

On the PC:

1. Install [Sunshine](https://github.com/LizardByte/Sunshine) and leave it on
   its default port (47989).
2. Install Tailscale and log in to the same tailnet as the console.

On the console:

1. Open the status page and find **Game streaming**.
2. Choose the device that runs Sunshine and press **Save**. The page shows
   "Forwarding 127.0.0.1 to *your-pc* (7 ports)".
3. In your Moonlight client on the PS5, add a host manually with the address
   **`127.0.0.1`**. Do not enter the PC's tailnet address: the client cannot
   reach it.
4. Pair as usual: the client shows a PIN, which you enter in Sunshine's web
   interface on the PC.

How it works: the daemon listens on `127.0.0.1` on Sunshine's ports (TCP
47984, 47989, 48010 and UDP 47998, 47999, 48000, 48002) and relays them to
the chosen host through Tailscale. To the Moonlight client the Sunshine host
appears to be the console itself.

Notes:

- One Sunshine host at a time. Change it on the status page at any time.
- A wired connection on the console helps, as with any streaming.
- **Do not change the console's network (Wi-Fi to Ethernet, connection
  settings) while a stream is running.** That froze the test console once;
  the cause was not established.
- Tested with ProsperoLight.

### Other apps on the console

`forwards` in the [configuration](#configuration) relays any localhost port
to a tailnet host in the same way, TCP or UDP.

The daemon also runs an HTTP proxy on `127.0.0.1:8118` that reaches tailnet
hosts, for apps that have their own proxy setting. **Do not set it as the
PS5's system proxy.** The system then sends everything through it, including
pages on `127.0.0.1`, and it is not running until Tailscale has been loaded.
On the test console that stopped another homebrew tool's page from opening.

## Configuration

`/data/tailscale/config.json` is created on first start. Every field is
optional. Restart Tailscale (send the payload again) to apply edits.

```json
{
  "hostname": "ps5",
  "authKey": "",
  "webAddr": ":8090",
  "httpProxyAddr": "127.0.0.1:8118",
  "controlURL": "",
  "sunshineHost": "",
  "forwards": [
    {"proto": "tcp", "listen": "127.0.0.1:8096", "target": "my-nas:8096"}
  ],
  "udpPorts": [9295, 9296, 9297, 9302],
  "blockedPorts": [],
  "verbose": false
}
```

| Field | Meaning |
| --- | --- |
| `hostname` | The console's name on the tailnet. |
| `authKey` | A Tailscale auth key, to log in without the browser step. |
| `webAddr` | Where the status page listens. |
| `httpProxyAddr` | Where the HTTP proxy listens. Empty turns it off. |
| `controlURL` | A coordination server other than Tailscale's. |
| `sunshineHost` | The Sunshine host; set from the status page. |
| `forwards` | Extra local forwards: `proto` is `tcp` or `udp`, `listen` a localhost address, `target` a tailnet host and port. |
| `udpPorts` | The console's UDP ports reachable from the tailnet. Default `[9295, 9296, 9297, 9302]` (Remote Play). `[]` turns inbound UDP off. |
| `blockedPorts` | Local TCP ports that are never exposed to the tailnet. |
| `verbose` | Put Tailscale's own log in the main log as well. |

Files on the console:

| Path | What |
| --- | --- |
| `/data/tailscale/config.json` | Settings. |
| `/data/tailscale/state/` | Tailscale's state, including the login. |
| `/data/tailscale/tailscale.log` | The daemon's log, rotated at 2 MB. |
| `/data/tailscale/tailscale-debug.log` | Tailscale's detailed log, up to 4 MB plus one older file. |
| `/data/tailscale/tailscale.elf` | The daemon payload. |
| `/user/app/TSCL00001/` | The home screen icon. |

## Uninstall

Press **Uninstall** on the status page. It deletes the daemon payload and
stops Tailscale. It asks whether to also log out and delete the saved login.

Two things are left to do by hand:

- Delete the home screen icon (Options button, then Delete).
- Remove the device in the Tailscale admin console.
- If you added the payload to an autoloader yourself, remove it there.

## Troubleshooting

- **The status page does not open on the console, but does from a PC.**
  Check that the PS5's proxy server setting is "Do Not Use".
- **The Moonlight client cannot find the host.** The host to add is
  `127.0.0.1`, and a Sunshine host must be selected on the status page.
  Sunshine must be on its default port.
- **"Not logged in" after logging in.** Press **Log in again** for a fresh
  link.
- **Something else.** `http://<console>:8090/api/logs?full=1` is the daemon's
  log and `/api/logs?debug=1` is Tailscale's detailed log. Please attach them
  to bug reports, after checking them for anything you consider private.

## Security

- The status page and its controls are unauthenticated.
- All listening TCP ports on the console, and the UDP ports in `udpPorts`,
  become reachable from your tailnet. That includes the payload loader, which
  runs anything sent to it. Use Tailscale ACLs if other people share your
  tailnet.
- The local forwards and the proxy listen on `127.0.0.1` only and are not
  exposed to the tailnet.

## Resource use

About 60 MB of memory and next to no CPU when idle. The daemon runs at the
lowest scheduling priority on at most 4 cores, so it gives way to games.

## What has and has not been tested

Tested on the one console: install and upgrade, login with device approval,
starting again after a reboot with the saved login, reaching the console over
the tailnet, a ProsperoLight stream from a Sunshine host through the forward,
the HTTP proxy, the home screen icon.

Remote Play through the tailnet address works with Chiaki and with Asobi on
iOS and Android.

Not tested: rest mode, Uninstall on a console, other firmware versions,
coordination servers other than Tailscale's.

## Building

See [docs/BUILDING.md](docs/BUILDING.md). The build runs on Windows with
PowerShell, clang, ps5-payload-sdk and a patched Go 1.27.1.

## How it works

`tailscale.elf` is a small C program built with
[ps5-payload-sdk](https://github.com/ps5-payload-dev/sdk) with a Go program
embedded in it. The SDK's startup code lifts the PS5's restriction on where
system calls may be issued from. The C part drops the process to the lowest
scheduling priority, maps the Go program into memory and enters it the way
the FreeBSD kernel would. Go's runtime and standard library are patched to
speak the PS5's FreeBSD 9 era system call interface. The Go program is a
[tsnet](https://pkg.go.dev/tailscale.com/tsnet) server that pipes every
incoming tailnet TCP connection to the same port on localhost.

[docs/TECHNICAL.md](docs/TECHNICAL.md) has the details, including what was
learned about running Go on the PS5.

## Credits

- [Tailscale](https://github.com/tailscale/tailscale), whose client this runs.
- John Törnblom's [ps5-payload-sdk](https://github.com/ps5-payload-dev/sdk)
  and [elfldr](https://github.com/ps5-payload-dev/elfldr).
- itsPLK's [Payload Manager](https://github.com/itsPLK/ps5-payload-manager),
  whose app-icon installer this follows.

## License

The project's own code is licensed under the GNU General Public License
version 3 or later (see [LICENSE](LICENSE)); the payloads link the SDK's
GPL-licensed startup code. `patches/` is a patch to the Go source tree and is
under Go's BSD-style license. Tailscale is BSD-3-Clause. The release binaries
also contain Tailscale's dependencies under their own licenses; they are
listed in `tsd/go.mod`.

"Tailscale" is a trademark of Tailscale Inc. "PlayStation" and "PS5" are
trademarks of Sony Interactive Entertainment Inc.
