# Troubleshooting

Commands below use `eth0` for the host's wired interface; `ip -br link` shows yours.

## Firmware

Setting names vary between board makers.

- **ErP / EuP Ready: disabled.** With it on, the network card loses standby power and cannot
  receive the magic packet.
- **Wake on LAN / Power On by PCI-E / PCIe Device Power On: enabled.**
- **Suspend to RAM.** `cat /sys/power/mem_sleep` must show `[deep]`. If only `s2idle` appears,
  look for "Suspend to RAM", "ACPI S3" or "Modern Standby" settings.
- The network card's LEDs should stay lit while the host is suspended. If they go dark,
  Wake-on-LAN cannot work.

## Wake-on-LAN

| Symptom | Check |
|---|---|
| Magic packet sent, host stays asleep | `ethtool eth0 \| grep Wake-on` must show `g`. Send one by hand from the proxy machine: `gpu-wake-proxy -wake`. From another LAN machine: `wakeonlan <mac>`. |
| Works once, never after a suspend or shutdown | TLP switches Wake-on-LAN off before sleep by default: `/etc/tlp.d/50-gpu-wake.conf` must set `WOL_DISABLE=N` (the installer writes it). `cat /sys/class/net/eth0/device/power/wakeup` must be `enabled`. |
| Works once, then not | NetworkManager reset it: `nmcli -g 802-3-ethernet.wake-on-lan con show "<connection>"` must be `magic`. The sleep hook re-arms it before each suspend and logs the state: `journalctl -t gpu-wake \| grep sleep:`. |
| Host was powered off, the wake did nothing | gpu-wake is built for suspend (S3). Waking from power-off (S5) needs its own firmware option, often called "Wake on LAN from S5". Set the desktop's power button to suspend. A cold boot takes longer before the first ping, so keep `wake_timeout_s` at 150 or more. |
| Wake works but the proxy never forwards | The app on the host is down or still loading, or the proxy cannot reach it: `curl <target><health>` from the proxy machine. |

## Suspend

| Symptom | Check |
|---|---|
| Never suspends | `journalctl -t gpu-wake -n 5` names the reason, such as `connections:2` or `gpu:35%`. `ss -tn state established '( sport = :8000 )'` shows who is connected. |
| Suspends right after waking | The sleep hook resets the idle clock on resume: `/usr/lib/systemd/system-sleep/gpu-wake` must exist, and the journal should show `sleep: resumed`. |
| `suspend: refused` in the log | `journalctl -u gpu-wake-idle` has systemd's reason. `suspend.target is masked` means sleep is switched off system-wide: `sudo systemctl unmask suspend.target sleep.target`. |
| Suspending takes long | VRAM in use is written to `/var/tmp` during suspend. Unload models first with `UNLOAD_URLS`, or accept the delay in exchange for a faster warm resume. `/var/tmp` needs at least as much free space as the VRAM in use. |
| Black screen or no CUDA after resume | `systemctl status nvidia-resume`. `grep Preserve /proc/driver/nvidia/params` must show 1. |

## Proxy

| Symptom | Check |
|---|---|
| Will not start | `gpu-wake-proxy -check -config /etc/gpu-wake/proxy.json`, then `journalctl -u gpu-wake-proxy`. |
| State stays `asleep` while the host is up | The proxy pings as an unprivileged user: `sysctl net.ipv4.ping_group_range` must include all groups (the installer sets `0 2147483647`). |
| Every visitor gets "too many wake requests" | The front proxy is not setting `X-Real-IP`, so all visitors share one address. |
