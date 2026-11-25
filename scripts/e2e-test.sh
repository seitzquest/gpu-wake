#!/usr/bin/env bash
# End-to-end wake test on real hardware. Run it on the proxy machine while the host is awake,
# then suspend the host (or let it idle-suspend):
#
#   nohup ./scripts/e2e-test.sh https://app.example.org 192.168.1.50 >e2e.log 2>&1 &
#
# Round 1 waits for the host to go down, wakes it like a visitor's browser does
# (POST /__gpu_wake/wake) and times the first ping, app readiness and one real request.
# Round 2 waits for the next idle suspend (up to IDLE_WAIT_S) and repeats.
#
# E2E_REQUEST: optional curl arguments for the timed request, for example
#   E2E_REQUEST='-X POST -H Content-Type:application/json -d {"prompt":"hi"} /api/generate'
set -uo pipefail
SITE=${1:?usage: $0 SITE_URL HOST_IP}
HOST=${2:?usage: $0 SITE_URL HOST_IP}
IDLE_WAIT_S=${IDLE_WAIT_S:-3600}
FIRST_WAIT_S=${FIRST_WAIT_S:-1800}
read -r -a REQUEST <<<"${E2E_REQUEST:-/}"

now() { date +%s.%N; }
since() { awk -v a="$1" -v b="$(now)" 'BEGIN { printf "%.1f", b - a }'; }
up() { ping -n -q -c1 -W1 "$HOST" >/dev/null 2>&1; }
say() { echo "$(date +%T) $*"; }

wait_down() {
  local limit=$1 start; start=$(now)
  while up; do
    sleep 1
    (( $(date +%s) - ${start%.*} > limit )) && { say "host still up after ${limit}s"; return 1; }
  done
  say "host down (after $(since "$start")s of waiting)"
}

wake_and_time() {
  local round=$1 t0 tp code path=${REQUEST[-1]}
  sleep 10   # let it settle in S3
  say "round $round: visitor opens the page"
  t0=$(now)
  code=$(curl -s -o /dev/null -w '%{http_code}' -H 'Accept: text/html' "$SITE/")
  say "round $round: page while asleep -> $code (waiting page expected: 503)"
  curl -s -X POST -H 'Accept: application/json' "$SITE/__gpu_wake/wake"; echo
  until up; do sleep 0.5; (( $(date +%s) - ${t0%.*} > 120 )) && { say "round $round: NO PING after 120s"; return 1; }; done
  say "round $round: ping after $(since "$t0")s"
  until curl -s -m 3 "$SITE/__gpu_wake/status" | grep -q '"ready":true'; do sleep 0.5
    (( $(date +%s) - ${t0%.*} > 300 )) && { say "round $round: app not ready after 300s"; return 1; }; done
  say "round $round: app ready through the proxy after $(since "$t0")s"
  tp=$(now)
  code=$(curl -s -m 300 -o /dev/null -w '%{http_code}' "${REQUEST[@]:0:${#REQUEST[@]}-1}" "$SITE$path")
  say "round $round: request $path -> $code in $(since "$tp")s; total from visit $(since "$t0")s"
}

say "e2e start: site=$SITE host=$HOST"
wait_down "$FIRST_WAIT_S" && wake_and_time 1
say "waiting for the idle auto-suspend (up to ${IDLE_WAIT_S}s)"
awake_at=$(now)
wait_down "$IDLE_WAIT_S" && { say "idle suspend $(since "$awake_at")s after round 1"; wake_and_time 2; }
say "e2e done"
