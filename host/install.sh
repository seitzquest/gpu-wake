#!/usr/bin/env bash
# Set up the GPU host: idle suspend, Wake-on-LAN, NVIDIA suspend support and the helpers.
# Run as root from a checkout: sudo ./host/install.sh
# Safe to re-run: it keeps the existing settings file and only changes what differs.
set -euo pipefail
cd "$(dirname "$0")"
[[ $EUID -eq 0 ]] || { echo "Run with sudo: sudo $0" >&2; exit 1; }

step() { printf '\n== %s\n' "$*"; }
warn() { printf 'WARNING: %s\n' "$*" >&2; }

step "Packages"
need=()
for cmd in ethtool curl; do command -v "$cmd" >/dev/null || need+=("$cmd"); done
command -v xprintidle >/dev/null || echo "xprintidle not found: desktop input will not keep the host awake (X11 only)"
if (( ${#need[@]} )); then
  if command -v pacman >/dev/null; then pacman -S --needed --noconfirm "${need[@]}"
  elif command -v apt-get >/dev/null; then apt-get install -y "${need[@]}"
  elif command -v dnf >/dev/null; then dnf install -y "${need[@]}"
  else echo "Install these first: ${need[*]}" >&2; exit 1; fi
fi
command -v nvidia-smi >/dev/null || warn "nvidia-smi not found: GPU checks and the power limit are skipped"

step "Settings"
install -d -m 0755 /etc/gpu-wake
conf=/etc/gpu-wake/host.conf
# Installs from before the rename keep their pc.conf; every script reads either.
[[ ! -e $conf && -e /etc/gpu-wake/pc.conf ]] && conf=/etc/gpu-wake/pc.conf
if [[ -e $conf ]]; then
  echo "keeping $conf"
  # Append settings introduced since it was written; existing values are never changed.
  while IFS= read -r line; do
    [[ $line =~ ^[A-Z_]+= ]] || continue
    grep -q "^${line%%=*}=" "$conf" || { echo "$line" >>"$conf"; echo "  added $line"; }
  done <host.conf.example
else
  install -m 0644 host.conf.example "$conf"
  echo "created $conf: edit WATCH_PORTS and the other settings for your apps"
fi
# shellcheck source=/dev/null
. "$conf"

step "Sleep state (S3)"
cat /sys/power/mem_sleep
if ! grep -q '\[deep\]' /sys/power/mem_sleep; then
  if grep -qw deep /sys/power/mem_sleep; then
    warn "deep is offered but not selected: add mem_sleep_default=deep to the kernel command line"
  else
    warn "deep (S3) is not offered: enable S3 or disable 'Modern Standby' in the firmware (docs/troubleshooting.md)"
  fi
fi

step "NVIDIA suspend support"
if command -v nvidia-smi >/dev/null; then
  if grep -rqs 'NVreg_PreserveVideoMemoryAllocations=1' /etc/modprobe.d/ \
     && grep -rqs 'NVreg_TemporaryFilePath=' /etc/modprobe.d/; then
    echo "modprobe options already set"
  else
    echo "options nvidia NVreg_PreserveVideoMemoryAllocations=1 NVreg_TemporaryFilePath=/var/tmp" \
      >/etc/modprobe.d/gpu-wake-nvidia.conf
    echo "wrote /etc/modprobe.d/gpu-wake-nvidia.conf (takes effect after a reboot; rebuild the initramfs if it includes nvidia)"
  fi
  grep -E 'PreserveVideoMemoryAllocations|TemporaryFilePath' /proc/driver/nvidia/params || true
  systemctl enable nvidia-suspend.service nvidia-resume.service nvidia-hibernate.service 2>/dev/null \
    || warn "could not enable nvidia-suspend/resume services"
fi

step "Wake-on-LAN"
iface=$(ip -o route show default | awk '$5 ~ /^(en|eth)/ {print $5; exit}')
if [[ -z $iface ]]; then
  warn "no wired default route; plug in Ethernet and re-run"
else
  if command -v nmcli >/dev/null && con=$(nmcli -g GENERAL.CONNECTION device show "$iface" 2>/dev/null) && [[ -n $con ]]; then
    if [[ $(nmcli -g 802-3-ethernet.wake-on-lan connection show "$con") != magic ]]; then
      nmcli connection modify "$con" 802-3-ethernet.wake-on-lan magic
      nmcli connection up "$con" >/dev/null
      echo "NetworkManager: magic-packet Wake-on-LAN on '$con' ($iface)"
    fi
  fi
  # TLP's default WOL_DISABLE=Y switches Wake-on-LAN off right before every suspend and
  # shutdown, overriding the NetworkManager setting at exactly the moment it is needed.
  if command -v tlp >/dev/null; then
    mkdir -p /etc/tlp.d
    printf '# gpu-wake: keep Wake-on-LAN armed across suspend and shutdown\nWOL_DISABLE=N\n' \
      >/etc/tlp.d/50-gpu-wake.conf
    echo "TLP: WOL_DISABLE=N (/etc/tlp.d/50-gpu-wake.conf)"
  fi
  ethtool -s "$iface" wol g
  ethtool "$iface" | grep -E 'Wake-on' || true
  wake=/sys/class/net/$iface/device/power/wakeup
  [[ -w $wake ]] && [[ $(<"$wake") != enabled ]] && echo enabled >"$wake"
  echo
  echo "For the proxy's host settings:"
  echo "  mac:       $(cat /sys/class/net/"$iface"/address)"
  echo "  ip:        $(ip -4 -br addr show "$iface" | awk '{print $3}' | cut -d/ -f1)  (reserve it in your router's DHCP)"
  echo "  broadcast: $(ip -4 -o addr show "$iface" | awk '{for (i = 1; i < NF; i++) if ($i == "brd") print $(i + 1)}')"
fi

step "Scripts and units"
install -m 0755 bin/gpu-wake-idle bin/stay-awake bin/gpu-exclusive /usr/local/bin/
install -d -m 0755 /etc/gpu-wake/checks.d
install -m 0755 sleep-hook /usr/lib/systemd/system-sleep/gpu-wake
install -m 0644 systemd/gpu-wake-idle.service systemd/gpu-wake-idle.timer \
  systemd/gpu-wake-power.service /etc/systemd/system/
cat >/etc/tmpfiles.d/gpu-wake.conf <<'EOF'
d /run/gpu-wake       0755 root root -
d /run/gpu-wake-flags 1777 root root -
EOF
systemd-tmpfiles --create /etc/tmpfiles.d/gpu-wake.conf
systemctl daemon-reload
if command -v nvidia-smi >/dev/null; then
  systemctl enable gpu-wake-power.service >/dev/null
  systemctl restart gpu-wake-power.service
  nvidia-smi --query-gpu=persistence_mode,power.limit --format=csv,noheader
fi
systemctl enable --now gpu-wake-idle.timer

# PI_* are the earlier names of the PROXY_* settings.
proxy_ip=${PROXY_LAN_IP:-${PI_LAN_IP:-}} ports=${PROXY_ONLY_PORTS:-${PI_ONLY_PORTS:-}}
if [[ -n $proxy_ip && -n $ports && -n ${LAN_CIDR:-} ]]; then
  step "Firewall (ufw): only the proxy reaches $ports over the LAN"
  if command -v ufw >/dev/null && ufw status | grep -q 'Status: active'; then
    for p in $ports; do
      ufw allow proto tcp from "$proxy_ip" to any port "$p" comment gpu-wake >/dev/null
      ufw deny proto tcp from "$LAN_CIDR" to any port "$p" comment gpu-wake >/dev/null
    done
    ufw status numbered | grep -E "gpu-wake|Status" || true
  else
    warn "ufw is not active; ports $ports are open to the whole LAN"
  fi
fi

step "Done"
echo "Logs: journalctl -t gpu-wake -f    Keep awake: stay-awake [3h|off|-- cmd]"
echo "What the idle check would do now:"; /usr/local/bin/gpu-wake-idle --dry-run || true
