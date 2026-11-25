# Integrating apps

gpu-wake works with any HTTP app. Taking part in the contract below is optional, and each part
makes waking, sleeping and sharing the GPU smoother.

## Health: when to forward

Set `health` to a path that answers as soon as the app's HTTP server runs, even while models
are still loading. The proxy forwards once it gets any status below 500, or 503. Until then,
browsers stay on the waiting page and API clients wait.

If loading takes a while, answer 503 from a readiness path and let your own page say "loading
the model". The waiting page then hands over to your app early, and your app explains the rest
truthfully.

## Bind to the host's address, not to everything

The proxy reaches the app at `target`. Bind the app to the LAN or VPN address the proxy uses,
and use `PROXY_ONLY_PORTS` (with ufw) if other LAN devices should not reach it directly.

## Busy: keeping the host awake without a connection

Long jobs that hold no open connection and use little GPU (an agent waiting on a tool, a queue
worker between batches) can keep the host awake by answering a URL listed in `BUSY_URLS`:

```json
{"busy": true}
```

Anything else, including an error or a timeout after 3 s, counts as not busy.

For a command you start yourself, `stay-awake -- your command` holds a sleep inhibitor while it
runs.

## Your own checks

For anything else, drop an executable into `/etc/gpu-wake/checks.d/`. If it exits 0, the host
stays awake, and the first line it prints is logged as the reason. Any other exit code, or no
answer within 10 seconds, counts as idle. [examples/checks.d](../examples/checks.d) has two
examples: a running backup process and a Jellyfin stream.

```sh
#!/bin/sh
# /etc/gpu-wake/checks.d/game-server
pgrep -x minecraft >/dev/null && echo "minecraft server up"
```

Run `gpu-wake-idle --dry-run` to see what the next check would decide, without unloading or
suspending anything.

## Scheduled jobs

A system timer with `WakeSystem=true` wakes the host from suspend when it fires; systemd sets
the hardware alarm. gpu-wake does two things for such timers:

- it does not suspend the host when the timer fires within `SCHEDULE_GRACE_MINUTES` (default 10)
- it keeps the host awake while the service the timer started is active

```ini
# /etc/systemd/system/nightly-eval.timer
[Timer]
OnCalendar=*-*-* 03:00
WakeSystem=true
Persistent=true

[Install]
WantedBy=timers.target
```

After the job ends, the host suspends again after the usual `IDLE_MINUTES`. Check that your
firmware supports waking from the real-time clock: `/sys/class/rtc/rtc0/wakealarm` must exist.
Use a system timer (with `User=` in its service if needed): timers in a user's own systemd
instance usually lack the privilege to wake the system, and gpu-wake only looks at system timers.

## Releasing GPU memory

Set `release_path` on an upstream to let the admin status page, or `POST /release name=...`,
ask the app to free its GPU memory. The proxy sends:

```
POST <release_path>
Host: localhost
Content-Type: application/json
Authorization: Bearer <GPU_CONTROL_TOKEN>    (only when the token is set)

{}
```

Answer `200` when the memory is released; the next request may load it again. Answer `409`
with `{"error": "..."}` when the app is busy and refuses. Any other status is reported as a
failure. The call never wakes the host.

Before an idle suspend, `gpu-wake-idle` also requests each URL in `UNLOAD_URLS`. Without that,
the NVIDIA driver copies VRAM in use to disk during suspend, which makes suspending slower. For
llama-swap, the URL is its `/unload` endpoint.

## Giving one job the whole GPU

`gpu-exclusive` wraps a command that needs all of the GPU:

1. With `EXCLUSIVE_NEED_MIB` set, it checks first that the job would fit once the services in
   `EXCLUSIVE_UNITS` are stopped. If it would not (because something else holds the GPU), it
   exits with code 75 and changes nothing.
2. It pauses `PAUSE_UPSTREAMS` on the proxy, so visitors see "GPU in use", and stops
   `EXCLUSIVE_UNITS`.
3. When the command exits, it starts the services again and lifts the pause.

As a llama-swap model command:

```yaml
models:
  "big-model":
    cmd: gpu-exclusive /usr/local/bin/llama-server --port ${PORT} -m /models/big.gguf
    env: ["EXCLUSIVE_NEED_MIB=22000"]
    ttl: 1200
```

`gpu-guard` covers jobs that were not started this way, such as a training run from a terminal.
Every 20 seconds it compares free VRAM (plus what each app already holds) with the app's need
from `GPU_GUARDS`, and pauses or releases its upstreams with 1 GiB of hysteresis.

## Themes

The waiting, busy, offline and status pages use a neutral built-in theme in light and dark.
There are two ways to make them match your site:

- **`pages.style_url`**: a stylesheet loaded after the theme. It can override the design tokens:

  ```css
  :root { --gw-accent: #0a7cff; --gw-font: "Inter", system-ui, sans-serif; --gw-radius: 2px; }
  ```

- **`pages.templates_dir`**: `*.html` files whose `{{define}}` blocks replace the built-in
  ones. The blocks are `theme` (the `--gw-*` tokens), `head`, `header` and `art` (the picture
  beside the message; `.Kind` is `waking`, `busy` or `offline`). For example, a file
  `mysite.html`:

  ```html
  {{define "header"}}<header class="gw-header"><a href="https://example.org/">example.org</a></header>{{end}}
  ```

The proxy reads the templates at startup, so restart it after changing them.
