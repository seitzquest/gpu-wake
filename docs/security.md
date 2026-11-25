# Security model

gpu-wake sits between the internet and a machine in your home, so it is designed to fail
closed and to expose as little as possible.

## What is exposed

- **Nothing directly.** Every listener binds to loopback; the config refuses any other address.
  Your front proxy decides what is public, and adds TLS and rate limits.
- **Per upstream:** the app itself, plus `/__gpu_wake/state`, `/__gpu_wake/status` and
  `POST /__gpu_wake/wake`. The state endpoint answers from memory and never contacts the host.
- **The admin listener** (status page, wake, kill switch, pause, release) should be reachable on
  a private network only. It rejects cross-site POSTs (`Sec-Fetch-Site`, `Origin`), so a web
  page open on a private device cannot change it.

## Who can wake the host

- On public upstreams, a GET never wakes the host. Crawlers, link previews and prefetches get the
  waiting page or a JSON 503 and nothing else. The waiting page's script, its no-JavaScript
  button or a non-GET request starts a wake.
- Each client IP may start `wakes_per_ip_per_hour` wakes. All public traffic together may start
  `public_wakes_per_hour`; beyond that, public visitors can use a host that is already awake but
  cannot wake it.
- `wake_policy: any` is allowed only on private upstreams.

The per-IP limit uses the `X-Real-IP` or `X-Forwarded-For` header. Since every request comes
through your front proxy on loopback, the front proxy must set `X-Real-IP` to the real client
address. Behind Cloudflare, that means restoring it from `CF-Connecting-IP` first. If the limiter
tracks more than 10,000 addresses, it forgets them; the hourly breaker still caps the total.

## Kill switch

`gpu-bridge public-off` makes public upstreams serve an offline page without contacting the
host. `gpu-bridge off` stops all forwarding and every wake. The mode is kept in `state_file`. If
that file cannot be read at startup, the proxy starts in `public-off`, never in `on`.

## Load

`max_inflight` caps concurrent forwarded requests per upstream; beyond it the proxy answers 503
at once instead of queueing work on the host. The systemd unit limits the proxy to 64 MB of
memory and runs it as a dynamic user with a read-only system.

## What gpu-wake does not do

- No authentication of visitors. Put authentication in your app or front proxy.
- No TLS. Terminate TLS in the front proxy.
- Pausing and releasing are cooperative. An app that ignores a release keeps its memory.
