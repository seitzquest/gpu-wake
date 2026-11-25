#!/usr/bin/env bash
# Set up the wake proxy on the always-on machine: build, install, configure and start it.
# Run as root from a checkout: sudo ./proxy/install.sh
# Exposure (nginx, Caddy, a tunnel, tailscale serve) is configured in your front proxy, not here.
set -euo pipefail
cd "$(dirname "$0")"
[[ $EUID -eq 0 ]] || { echo "Run with sudo: sudo $0" >&2; exit 1; }

echo "== Build"
if [[ -x gpu-wake-proxy ]]; then
  echo "using the prebuilt ./gpu-wake-proxy"
else
  command -v go >/dev/null || { echo "Go 1.24 or newer is required, or put a release binary at proxy/gpu-wake-proxy" >&2; exit 1; }
  owner=$(stat -c %U .)
  version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
  # Build as the checkout owner so the Go cache stays in their home.
  runuser -u "$owner" -- env CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=$version" -o gpu-wake-proxy .
fi
install -m 0755 gpu-wake-proxy /usr/local/bin/gpu-wake-proxy
install -m 0755 gpu-bridge /usr/local/bin/gpu-bridge
/usr/local/bin/gpu-wake-proxy -version

echo "== Config"
install -d -m 0755 /etc/gpu-wake
if [[ -e /etc/gpu-wake/proxy.json ]]; then
  echo "keeping /etc/gpu-wake/proxy.json"
else
  install -m 0644 proxy.json.example /etc/gpu-wake/proxy.json
  echo "created /etc/gpu-wake/proxy.json from the example: set host and upstreams, then re-run"
fi
/usr/local/bin/gpu-wake-proxy -check -config /etc/gpu-wake/proxy.json

echo "== ICMP for the unprivileged service user"
range=$(sysctl -n net.ipv4.ping_group_range)
if [[ $range != "0	2147483647" && $range != "0 2147483647" ]]; then
  echo "net.ipv4.ping_group_range = 0 2147483647" >/etc/sysctl.d/60-gpu-wake-ping.conf
  sysctl -q -p /etc/sysctl.d/60-gpu-wake-ping.conf
fi

echo "== Service"
install -m 0644 gpu-wake-proxy.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable gpu-wake-proxy.service >/dev/null
systemctl restart gpu-wake-proxy.service
sleep 1
systemctl --no-pager --lines=8 status gpu-wake-proxy.service | sed -n '1,12p'
echo
echo "Status: gpu-bridge status    Kill switch: gpu-bridge off|public-off|on    Logs: journalctl -u gpu-wake-proxy -f"
