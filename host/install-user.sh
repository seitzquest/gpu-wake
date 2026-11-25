#!/usr/bin/env bash
# User-level part of the host setup (no sudo): gpu-guard, its timer and a user copy of gpu-exclusive.
# Run as the user who owns the GPU services: ./host/install-user.sh
set -euo pipefail
cd "$(dirname "$0")"
[[ $EUID -ne 0 ]] || { echo "Run as your user, not root" >&2; exit 1; }
install -D -m 0755 bin/gpu-guard "$HOME/.local/bin/gpu-guard"
# User copy of gpu-exclusive for user services such as llama-swap (updates need no sudo).
install -D -m 0755 bin/gpu-exclusive "$HOME/.local/bin/gpu-exclusive"
mkdir -p "$HOME/.local/state/gpu-guard"
install -D -m 0644 systemd/gpu-guard.service systemd/gpu-guard.timer -t "$HOME/.config/systemd/user/"
systemctl --user daemon-reload
systemctl --user enable --now gpu-guard.timer
"$HOME/.local/bin/gpu-guard"
echo "gpu-guard installed. Logs: journalctl --user -t gpu-guard -f"
