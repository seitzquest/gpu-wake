# Changelog

## v0.1.0 (unreleased)

First public release.

- `gpu-wake-proxy`: a wake-on-demand reverse proxy with a waiting page, streaming, shared wakes,
  explicit and any wake policies, per-IP and hourly wake limits, a persistent kill switch,
  pauses, GPU release calls and an admin status page.
- Host: idle suspend with logged reasons, `stay-awake`, `gpu-exclusive`, `gpu-guard`, a sleep
  hook that re-arms Wake-on-LAN, and the GPU power limit.
- Your own keep-awake checks in `/etc/gpu-wake/checks.d`, and `gpu-wake-idle --dry-run`.
- Scheduled jobs: systemd timers with `WakeSystem=true` are respected, before they fire and while
  their job runs.
- The host installer reports which app ports actually accept connections from the LAN.
- Neutral light and dark pages, themeable through `pages.style_url` and `pages.templates_dir`.
- Release archives for linux/arm64, linux/armv7 and linux/amd64.

### Coming from the private version

- The config key `pc` is now `host`, `style_url` moved to `pages.style_url`, and
  `audience: tailnet` is now `audience: private`. The old names are still accepted.
- The host settings file is `/etc/gpu-wake/host.conf`; an existing `pc.conf` is still read.
  `LLAMA_SWAP_URL` became `UNLOAD_URLS`, `AGENT_NEED_MIB` became `STATUS_NEED_MIB`, and
  `PI_LAN_IP` and `PI_ONLY_PORTS` became `PROXY_LAN_IP` and `PROXY_ONLY_PORTS`. The old names
  still work.
- The scripts no longer carry defaults for particular apps. `GPU_GUARDS`, `EXCLUSIVE_UNITS` and
  `STATUS_FREEABLE_UNITS` must now be set in the settings file.
