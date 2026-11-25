#!/usr/bin/env bash
# Tests for gpu-wake-idle's decisions, run with fake system commands and --dry-run, so nothing
# is ever unloaded or suspended. Usage: host/test/idle_test.sh
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
idle=$here/../bin/gpu-wake-idle
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/checks.d"

# Fake commands, steered by environment variables.
cat >"$tmp/bin/ss" <<'EOF'
#!/bin/sh
i=0; while [ "$i" -lt "${FAKE_CONNECTIONS:-0}" ]; do echo "ESTAB 0 0 10.0.0.2:8000 10.0.0.3:5000"; i=$((i + 1)); done
EOF
cat >"$tmp/bin/nvidia-smi" <<'EOF'
#!/bin/sh
case "$*" in
  *pmon*) printf '# gpu pid type sm\n# Idx # C/G %%\n0 123 C %s\n' "${FAKE_GPU:-0}" ;;
  *) echo "${FAKE_GPU:-0}" ;;
esac
EOF
cat >"$tmp/bin/systemd-inhibit" <<'EOF'
#!/bin/sh
exit 0
EOF
cat >"$tmp/bin/systemctl" <<'EOF'
#!/bin/sh
# One fake timer, nightly.timer, which triggers nightly.service.
case "$1" in
  list-units) [ -n "${FAKE_TIMER_IN_S:-}" ] && echo "nightly.timer loaded active waiting Nightly job" ;;
  show)
    echo "WakeSystem=${FAKE_TIMER_WAKE:-yes}"
    echo "NextElapseUSecRealtime=@$(( $(date +%s) + ${FAKE_TIMER_IN_S:-0} ))"
    echo "Triggers=nightly.service" ;;
  is-active) [ "${FAKE_JOB_ACTIVE:-0}" = 1 ] ;;
  is-enabled) echo enabled ;;
  *) exit 0 ;;
esac
EOF
chmod +x "$tmp/bin/"*

cat >"$tmp/host.conf" <<EOF
IDLE_MINUTES=15
GPU_UTIL_THRESHOLD=10
WATCH_PORTS="8000"
SSH_PORT=22
DESKTOP_USER=""
CHECKS_DIR=$tmp/checks.d
SCHEDULE_GRACE_MINUTES=10
EOF

failures=0
expect() {  # name, expected substring, then VAR=value settings
  local name=$1 want=$2; shift 2
  local out
  out=$(env "$@" PATH="$tmp/bin:$PATH" TMPDIR="$tmp" GPU_WAKE_CONF="$tmp/host.conf" "$idle" --dry-run 2>&1)
  if [[ $out == *"$want"* ]]; then
    echo "ok   $name"
  else
    echo "FAIL $name: want '$want', got: $out"
    failures=$(( failures + 1 ))
  fi
}
idle_for() { echo $(( $(date +%s) - $1 )) >"$tmp/gpu-wake-dry-run-$(id -u)/busy-at"; }

expect "fresh start counts as idle time" "awake: idle"
expect "connection keeps it awake" "connections:2" FAKE_CONNECTIONS=2
expect "compute load keeps it awake" "gpu:55%" FAKE_GPU=55
expect "load below the threshold does not" "awake: idle" FAKE_GPU=5

printf '#!/bin/sh\necho "backup running"\n' >"$tmp/checks.d/backup"
chmod +x "$tmp/checks.d/backup"
expect "a check that exits 0 keeps it awake, with its reason" "check:backup running"
printf '#!/bin/sh\nexit 1\n' >"$tmp/checks.d/backup"
expect "a check that exits 1 does not" "awake: idle"
printf '#!/bin/sh\nexit 0\n' >"$tmp/checks.d/quiet"
chmod +x "$tmp/checks.d/quiet"
expect "a silent check is named after its file" "check:quiet"
rm -f "$tmp/checks.d/quiet"
printf '#!/bin/sh\nsleep 30\n' >"$tmp/checks.d/slow"
chmod +x "$tmp/checks.d/slow"
SECONDS=0
expect "a check that hangs is cut off and ignored" "awake: idle"
(( SECONDS < 20 )) || { echo "FAIL hanging check took ${SECONDS}s"; failures=$(( failures + 1 )); }
rm -f "$tmp/checks.d/slow"

expect "a wake timer due soon keeps it awake" "timer:nightly.timer in 5m" FAKE_TIMER_IN_S=300
expect "a wake timer due later does not" "awake: idle" FAKE_TIMER_IN_S=3600
expect "a timer without WakeSystem is ignored" "awake: idle" FAKE_TIMER_IN_S=300 FAKE_TIMER_WAKE=no
expect "the job a wake timer started keeps it awake" "job:nightly.service" FAKE_TIMER_IN_S=86000 FAKE_JOB_ACTIVE=1

idle_for 1000
expect "idle past IDLE_MINUTES would suspend" "dry run: would unload and suspend now"
idle_for 1000
expect "but not with a wake timer due soon" "timer:nightly.timer" FAKE_TIMER_IN_S=120

if (( failures > 0 )); then echo "$failures failed"; exit 1; fi
echo "all idle tests passed"
