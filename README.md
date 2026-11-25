# gpu-wake

Let a GPU machine sleep, and wake it when someone needs it.

gpu-wake serves web apps and APIs from a Linux GPU machine that suspends when idle. A small
always-on machine on the same LAN (a Raspberry Pi is plenty) receives the requests. If the GPU
host is asleep, the proxy wakes it with Wake-on-LAN, shows browsers a waiting page, holds API
requests until the app is ready, then forwards everything with streaming intact.

```
visitor ─▶ front proxy ─▶ gpu-wake-proxy ─┬─ Wake-on-LAN, if asleep ───▶ GPU host
           TLS, routing   always on,      │                              your apps
                          for example     └─ HTTP, once the app is up ─▶ gpu-wake-idle suspends
                          a Raspberry Pi                                 it after N idle minutes
```

## What you get

- **A wake proxy.** One static Go binary with no dependencies, using about 10 MB of memory.
  Concurrent requests share one wake. Responses and server-sent events stream without
  buffering. The proxy watches the host by ping only, so watching never keeps it awake.
- **Idle suspend on the host.** A timer checks once a minute for open connections to your apps,
  CUDA load, SSH sessions, desktop input, sleep inhibitors and apps that report themselves
  busy. Every decision is logged with its reasons.
- **Safe on the public internet.** On public sites only an explicit action wakes the host, so
  crawlers and link previews never do. Wakes are limited per IP and per hour, and a kill switch
  cuts the host off in one command.
- **Sharing one GPU.** `gpu-exclusive` gives a job the whole GPU and restores your apps
  afterwards. `gpu-guard` shows "GPU in use" instead of an out-of-memory error. Apps can expose
  an endpoint that releases their GPU memory on request.

## Requirements

| | GPU host | Proxy machine |
|---|---|---|
| OS | Linux with systemd | Linux with systemd (Raspberry Pi OS, Debian, ...) |
| Network | Wired Ethernet with Wake-on-LAN | Same LAN as the host |
| Power | Suspend to RAM (S3, `deep`) | Always on |
| Other | NVIDIA GPU for GPU checks (optional), `ethtool`, `curl` | `ping`; Go 1.24+ only to build from source |

TLS, hostnames and rate limits per path belong to your front proxy (nginx, Caddy, a Cloudflare
Tunnel, `tailscale serve`). The wake proxy listens on loopback only and refuses any other address.

## Quick start

**1. The GPU host.** From a checkout:

```bash
sudo ./host/install.sh            # idle suspend, Wake-on-LAN, NVIDIA suspend support
sudoedit /etc/gpu-wake/host.conf  # WATCH_PORTS: the ports your apps listen on
```

The installer prints the MAC, IP and broadcast address the proxy needs. Reserve the IP in your
router's DHCP settings, and check the firmware settings in
[troubleshooting](docs/troubleshooting.md#firmware) before relying on Wake-on-LAN.

**2. The proxy machine.** Download a [release](../../releases) and unpack it, or build from source:

```bash
sudo ./proxy/install.sh               # installs gpu-wake-proxy and creates /etc/gpu-wake/proxy.json
sudoedit /etc/gpu-wake/proxy.json     # host: mac, ip, broadcast; one upstream per app
sudo systemctl restart gpu-wake-proxy
```

A minimal config, for one private API that wakes the host on any request:

```json
{
  "host": {"mac": "00:11:22:33:44:55", "ip": "192.168.1.50", "broadcast": "192.168.1.255"},
  "admin_listen": "127.0.0.1:9180",
  "upstreams": [
    {"name": "llm", "listen": "127.0.0.1:9111", "target": "http://192.168.1.50:8080",
     "health": "/health", "audience": "private", "wake_policy": "any"}
  ]
}
```

**3. The front proxy.** Point each hostname at its upstream's `listen` address and pass the
client address in `X-Real-IP`. There are complete examples for
[nginx](examples/nginx.conf) and [Caddy](examples/Caddyfile).

**4. Try it.** Suspend the host (`systemctl suspend`) and open the site. You should see the
waiting page, then your app. `journalctl -u gpu-wake-proxy -f` on the proxy machine shows the
wake, and `journalctl -t gpu-wake -f` on the host shows each idle decision.

## How it decides

**Waking.** Each upstream has an audience and a wake policy:

| | `wake_policy: explicit` (default) | `wake_policy: any` |
|---|---|---|
| `audience: public` (default) | Browsers get a waiting page whose script requests the wake. A GET without JavaScript never wakes the host. | Not allowed |
| `audience: private` | As above | Any request wakes the host and waits for the app |

Public wakes count against `wakes_per_ip_per_hour` and `public_wakes_per_hour`. Past the second
limit, public visitors can still use a host that is awake but cannot wake it.

**Staying awake.** The host suspends after `IDLE_MINUTES` with none of these:

- established connections to `WATCH_PORTS` or to SSH
- CUDA utilisation above `GPU_UTIL_THRESHOLD` (desktop graphics don't count)
- keyboard or mouse use in the X11 session of `DESKTOP_USER`
- `stay-awake` (`stay-awake 3h`, `stay-awake -- python train.py`)
- a blocking sleep inhibitor, or an idle inhibitor from a video player
- a `BUSY_URLS` endpoint answering `"busy":true`
- one of your own checks in `/etc/gpu-wake/checks.d` exiting 0 (examples in
  [examples/checks.d](examples/checks.d))
- a system timer with `WakeSystem=true` due within `SCHEDULE_GRACE_MINUTES`, or the job it started
  still running

**Scheduled jobs.** Give a systemd timer `WakeSystem=true`, and systemd wakes the host for it.
gpu-wake then keeps the host from suspending just before the timer fires, and keeps it awake
while the job runs. `gpu-wake-idle --dry-run` shows what the next check would decide, and why.

**Kill switch.** On the proxy machine, `gpu-bridge public-off "reason"` serves an offline page to
public upstreams and `gpu-bridge off` stops everything. `gpu-bridge on` turns it back on. The mode
survives reboots.

## How it compares

Several tools cover part of this. Choose the smaller one if it covers what you need:

| | Wakes and proxies | Waiting page | Safe for public sites | Suspends the host | GPU sharing |
|---|---|---|---|---|---|
| **gpu-wake** | yes | yes | only explicit wakes, per-IP and hourly limits, kill switch | yes, with logged reasons | pause, release, exclusive jobs |
| [llama-swap wol-proxy](https://github.com/mostlygeek/llama-swap/tree/main/cmd/wol-proxy) | yes | loading page; optional "Start server" confirmation | confirmation mode | no | no |
| [caddy-wol](https://github.com/dulli/caddy-wol) | yes, inside Caddy | no, the request waits | one wake per 10 minutes per client | no | no |
| [wakezilla](https://docs.rs/crate/wakezilla/latest) | yes, plus TCP ports | no | not described | powers off idle machines | no |
| [Sablier](https://github.com/sablierapp/sablier) | starts containers, no Wake-on-LAN | yes, themeable | not described | stops containers, not the host | anti-affinity groups |
| [autosuspend](https://autosuspend.readthedocs.io/) | no | no | n/a | yes, many checks and scheduled wake-ups | no |

- If you run llama-swap for your own use on a LAN, its wol-proxy may be all you need.
- If Caddy already fronts everything and a slow first load is fine, caddy-wol is one directive.
- If you need many host-side checks but no proxy, autosuspend is more complete on that side.

gpu-wake is for serving apps from a sleeping GPU machine to the public internet. It handles both
machines, and it shares one GPU between demos and larger jobs.

## Documentation

- [Configuration](docs/configuration.md): every setting in `proxy.json` and `host.conf`
- [Integrating apps](docs/integrating-apps.md): health checks, busy reporting, releasing GPU
  memory, `gpu-exclusive` with llama-swap, themes for the waiting pages
- [Security model](docs/security.md): what is exposed, what is trusted, and the limits
- [Troubleshooting](docs/troubleshooting.md): Wake-on-LAN, suspend and resume problems

## Limitations

- Linux and systemd on both machines. One GPU host per proxy.
- Wake-on-LAN needs the proxy on the host's LAN segment; it does not cross a VPN.
- Desktop input is detected on X11 only.
- Pausing and releasing are cooperative. They depend on apps and jobs taking part, and they
  are not memory isolation.

## License

[MIT](LICENSE)
