# Configuration

## Proxy: `/etc/gpu-wake/proxy.json`

Check a file with `gpu-wake-proxy -check -config FILE`. Unknown keys are errors, so typos are
caught at startup. A full example is [proxy/proxy.json.example](../proxy/proxy.json.example).

### Top level

| Key | Default | Meaning |
|---|---|---|
| `host.mac` | required | MAC address of the host's wired interface |
| `host.ip` | required | Host address the proxy pings. Use the LAN address; reserve it in DHCP. |
| `host.broadcast` | required | LAN broadcast address for the magic packet, such as `192.168.1.255` |
| `host.iface` | system default | Interface the magic packet leaves from, such as `eth0` |
| `wake_timeout_s` | 150 | How long one wake may take, including the app's startup. A cold boot takes longer than a resume. |
| `resend_interval_s` | 5 | Magic packets repeat at this interval until the host answers pings |
| `healthy_cache_s` | 10 | How long a successful health check is trusted |
| `wakes_per_ip_per_hour` | 6 | Wake requests allowed per client IP on public upstreams; 0 disables |
| `public_wakes_per_hour` | 6 | Wakes all public traffic may start per hour (the breaker); 0 disables |
| `admin_listen` | none | Loopback address of the admin listener (status page and controls) |
| `state_file` | `/var/lib/gpu-wake/bridge.json` | Where the kill-switch mode is kept across restarts |

### `pages`

| Key | Meaning |
|---|---|
| `site_name`, `site_url` | Optional breadcrumb on the waiting pages: `site_name / app title`, linking to `site_url` |
| `style_url` | Optional stylesheet loaded after the built-in theme. It can override the `--gw-*` design tokens. |
| `templates_dir` | Optional directory of `*.html` files whose `{{define}}` blocks replace the built-in ones. See [themes](integrating-apps.md#themes). |

### `upstreams[]`

| Key | Default | Meaning |
|---|---|---|
| `name` | required | Identifier used in logs, `/pause` and `/release` |
| `title` | `name` | Shown on the waiting pages |
| `listen` | required | Loopback `host:port` your front proxy forwards to |
| `target` | required | The app on the host, such as `http://192.168.1.50:8000` |
| `health` | `/` | Path that answers once the app's HTTP server is up. Any status below 500, and 503, counts as up. |
| `audience` | `public` | `public` or `private`. Public upstreams are subject to the wake limits and to `public-off`. |
| `wake_policy` | `explicit` | `explicit`: only the waiting page's request, or a non-GET request, wakes the host. `any`: every request does; private upstreams only. |
| `max_inflight` | 32 | Concurrent forwarded requests; more get an immediate 503 |
| `release_path` | none | App endpoint that frees the app's GPU memory, called from the admin listener |

Earlier versions used `pc` for `host`, top-level `style_url` and `audience: tailnet`. These are
still accepted.

### Admin listener

Serve it only on a private network (for example with `tailscale serve`). POSTs from other sites
are refused.

| Request | Effect |
|---|---|
| `GET /` | Status page |
| `GET /status` | The same as JSON |
| `POST /wake` | Wake the host now |
| `POST /bridge` `mode=on\|public-off\|off` `reason=...` | Kill switch, kept across restarts |
| `POST /breaker/reset` | Allow public wakes again before the hour is over |
| `POST /pause` `names=a,b` `paused=true\|false` `reason=...` | Show "GPU in use" for these upstreams; lifts itself after 6 hours |
| `POST /release` `name=a` | Call the upstream's `release_path`. Never wakes the host. |

Set `GPU_CONTROL_TOKEN` in `/etc/gpu-wake/control.env` to send it as a bearer token with release
calls.

Each upstream also answers a reserved path prefix, `/__gpu_wake/`, used by the waiting page:
`state` (from memory, never contacts the host), `status` (includes a health check) and
`POST wake`.

## Host: `/etc/gpu-wake/host.conf`

A shell file read by the host scripts. Installs from before the rename keep `pc.conf`, which is
read when `host.conf` does not exist. Changes apply at the next idle check.

| Setting | Used by | Meaning |
|---|---|---|
| `IDLE_MINUTES` | gpu-wake-idle | Suspend after this many idle minutes |
| `GPU_UTIL_THRESHOLD` | gpu-wake-idle | CUDA utilisation (%) above which the GPU counts as busy |
| `WATCH_PORTS` | gpu-wake-idle | Established connections to these ports keep the host awake |
| `SSH_PORT` | gpu-wake-idle | SSH sessions keep the host awake |
| `DESKTOP_USER` | gpu-wake-idle | User whose X11 keyboard and mouse input keeps the host awake |
| `BUSY_URLS` | gpu-wake-idle | URLs answering `"busy":true` while work is in progress |
| `CHECKS_DIR` | gpu-wake-idle | Executables that keep the host awake by exiting 0 (default `/etc/gpu-wake/checks.d`) |
| `SCHEDULE_GRACE_MINUTES` | gpu-wake-idle | Stay awake when a `WakeSystem=true` timer fires within this many minutes (default 10) |
| `UNLOAD_URLS` | gpu-wake-idle | Requested before suspend so model servers free VRAM (llama-swap: `/unload`) |
| `POWER_LIMIT_W` | gpu-wake-power | GPU power limit while awake; empty keeps the driver default |
| `PAUSE_URL` | gpu-exclusive, gpu-guard | The proxy's admin listener as seen from the host |
| `EXCLUSIVE_UNITS`, `PAUSE_UPSTREAMS` | gpu-exclusive | User services to stop and upstreams to pause for an exclusive job |
| `GPU_GUARDS` | gpu-guard | `name:user-unit:need-MiB:upstream,upstream` per app |
| `STATUS_NEED_MIB`, `STATUS_FREEABLE_UNITS` | gpu-guard | Write a status file saying whether a job of this size could start now |
| `PROXY_LAN_IP`, `LAN_CIDR`, `PROXY_ONLY_PORTS` | install.sh | With ufw active: only the proxy reaches these ports over the LAN |
