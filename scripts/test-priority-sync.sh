#!/usr/bin/env bash
# Verifies real-world priority behavior end-to-end using two REAL bearer
# tokens against a running gateway sitting behind APISIX: one token from a
# caller with the tier-5 (priority) role, one from an ordinary caller.
#
# APISIX derives X-Consumer-Priority purely from the token's Zitadel role and
# strips any client-supplied value up front (anti-spoof) — so this script
# cannot set the priority header itself. Priority can only be earned by
# presenting a token that actually carries the privileged role.
#
# For each of ROUNDS rounds:
#   1. Launch FILLER_COUNT background workers that continuously re-fire
#      NORMAL_TOKEN requests for SATURATE_SECONDS, keeping the shared sync
#      pool saturated for the whole window (not a single one-shot race that
#      can finish before the probes fire — a real backend's response time
#      can vary, and a one-shot filler racing a fixed sleep is inherently
#      flaky; continuous saturation removes that flakiness).
#   2. After WARMUP_SECONDS (just long enough for a worker to have landed and
#      taken the slot), fire NORMAL_PROBE_COUNT probes with NORMAL_TOKEN,
#      PRIORITY_COUNT probes with PRIORITY_TOKEN, and one "spoof" probe —
#      NORMAL_TOKEN plus a forged X-Consumer-Priority header set client-side —
#      all concurrently, while saturation is still running.
#
# The spoof probe must behave exactly like an ordinary normal request: if it
# ever gets 200 while a genuine normal probe was rejected (503) in the same
# round, APISIX is not stripping the header and priority can be self-granted.
#
# The script FAILS unless every priority-token probe succeeded (200) in every
# round, and no spoof probe ever bypassed the shared pool.
#
# Requires the target model to be configured with a small max_concurrent_sync
# and a priority_reserved_sync > 0, e.g.:
#   max_concurrent_sync: 2
#   priority_reserved_sync: 1
#
# Usage:
#   GATEWAY_URL=https://kevent-ai.example.com \
#   MODEL=whisper-diarization \
#   ENDPOINT=/v1/audio/transcriptions \
#   MULTIPART_FILE=/path/to/sample.wav \
#   PRIORITY_TOKEN="eyJ..." \
#   NORMAL_TOKEN="eyJ..." \
#   ./scripts/test-priority-sync.sh
#
# Env vars:
#   GATEWAY_URL         (required) base URL of the gateway, no trailing slash
#   MODEL               (required) model name to send in the request
#   PRIORITY_TOKEN       (required) bearer token for a tier-5 (priority) caller
#   NORMAL_TOKEN         (required) bearer token for a non-tier-5 (normal) caller
#   ENDPOINT             sync path to hit (default: /v1/chat/completions)
#   FILLER_COUNT         background saturation workers per round (default: 2 —
#                        should be >= the model's shared pool size, i.e.
#                        max_concurrent_sync - priority_reserved_sync)
#   SATURATE_SECONDS     how long each round keeps the shared pool saturated
#                        (default: 3). Must comfortably exceed WARMUP_SECONDS
#                        plus the probes' own response time.
#   WARMUP_SECONDS        delay after starting saturation workers before firing
#                        the probes, so a worker has actually landed and taken
#                        the slot (default: 0.3)
#   PRIORITY_COUNT       priority-token probes fired per round (default: 1)
#   NORMAL_PROBE_COUNT   normal-token probes fired per round (default: 1)
#   ROUNDS               number of repeated rounds (default: 5)
#   SPOOF_HEADER         header name a malicious client would try to forge
#                        (default: X-Consumer-Priority)
#   SPOOF_VALUE          value to forge it with (default: high)
#   BODY_FILE            path to a JSON request body file; overrides the
#                        default minimal chat body. Ignored when MULTIPART_FILE is set.
#   MULTIPART_FILE        path to a file to upload as multipart/form-data
#                        (e.g. a .wav/.mp3) for audio/vision/OCR endpoints
#   CURL_MAX_TIME         per-request curl timeout in seconds (default: 30)

set -euo pipefail

: "${GATEWAY_URL:?set GATEWAY_URL, e.g. https://kevent-ai.example.com}"
: "${MODEL:?set MODEL, e.g. whisper-diarization}"
: "${PRIORITY_TOKEN:?set PRIORITY_TOKEN, a bearer token for a tier-5 (priority) caller}"
: "${NORMAL_TOKEN:?set NORMAL_TOKEN, a bearer token for a non-tier-5 (normal) caller}"
ENDPOINT="${ENDPOINT:-/v1/chat/completions}"
FILLER_COUNT="${FILLER_COUNT:-2}"
SATURATE_SECONDS="${SATURATE_SECONDS:-3}"
WARMUP_SECONDS="${WARMUP_SECONDS:-0.3}"
PRIORITY_COUNT="${PRIORITY_COUNT:-1}"
NORMAL_PROBE_COUNT="${NORMAL_PROBE_COUNT:-1}"
ROUNDS="${ROUNDS:-5}"
SPOOF_HEADER="${SPOOF_HEADER:-X-Consumer-Priority}"
SPOOF_VALUE="${SPOOF_VALUE:-high}"
BODY_FILE="${BODY_FILE:-}"
MULTIPART_FILE="${MULTIPART_FILE:-}"
CURL_MAX_TIME="${CURL_MAX_TIME:-30}"

URL="${GATEWAY_URL%/}${ENDPOINT}"

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

if [[ -n "$MULTIPART_FILE" ]]; then
  common_curl_args=(-s -w '%{http_code} %{time_total}' --max-time "$CURL_MAX_TIME"
    -X POST "$URL" -F "model=$MODEL" -F "file=@${MULTIPART_FILE}")
else
  if [[ -n "$BODY_FILE" ]]; then
    body_file="$BODY_FILE"
  else
    body_file="$tmpdir/body.json"
    printf '{"model":"%s","messages":[{"role":"user","content":"ping"}]}' "$MODEL" > "$body_file"
  fi
  common_curl_args=(-s -w '%{http_code} %{time_total}' --max-time "$CURL_MAX_TIME"
    -X POST "$URL" -H "Content-Type: application/json" --data-binary "@$body_file")
fi

fire() {
  local label="$1" token="$2" spoof="$3"
  local args=("${common_curl_args[@]}" -H "Authorization: Bearer ${token}")
  if [[ "$spoof" == "1" ]]; then
    args+=(-H "${SPOOF_HEADER}: ${SPOOF_VALUE}")
  fi
  args+=(-o "$tmpdir/body_${label}_$$_${RANDOM}")
  local result
  result="$(curl "${args[@]}" || echo "000 0")"
  echo "$label $result" >> "$tmpdir/results"
}

# Repeatedly fires filler requests against the shared pool until now >= deadline.
# Whole-second granularity (not date +%s%N) so this works with stock macOS/BSD
# date, not just GNU coreutils — SATURATE_SECONDS must be a whole number.
saturate() {
  local worker="$1" deadline="$2"
  local n=0
  while [[ "$(date +%s)" -lt "$deadline" ]]; do
    n=$((n + 1))
    fire "filler-${worker}-${n}" "$NORMAL_TOKEN" 0
    # A rejected (503) attempt returns almost instantly, so without a pause
    # this loop would spin as fast as the network allows — dozens of
    # requests/sec per worker against a real gateway, an unintentional
    # mini load-test on every run. This caps it to a reasonable rate while
    # still refiring often enough to catch the pool as soon as it frees up.
    sleep 0.05
  done
}

echo "==> Target: $URL (model=$MODEL)"
echo "==> $ROUNDS round(s): $FILLER_COUNT saturation worker(s) for ${SATURATE_SECONDS}s -> warmup ${WARMUP_SECONDS}s -> $NORMAL_PROBE_COUNT normal + $PRIORITY_COUNT priority + 1 spoof probe(s)"
echo

priority_total=0
priority_ok=0
overall_pass=1

for round in $(seq 1 "$ROUNDS"); do
  : > "$tmpdir/results"
  echo "--- round $round/$ROUNDS ---"

  deadline=$(( $(date +%s) + SATURATE_SECONDS ))
  saturator_pids=()
  for i in $(seq 1 "$FILLER_COUNT"); do
    saturate "$i" "$deadline" &
    saturator_pids+=("$!")
  done

  sleep "$WARMUP_SECONDS"
  for i in $(seq 1 "$NORMAL_PROBE_COUNT"); do
    fire "normal-$i" "$NORMAL_TOKEN" 0 &
  done
  for i in $(seq 1 "$PRIORITY_COUNT"); do
    fire "priority-$i" "$PRIORITY_TOKEN" 0 &
  done
  fire "spoof" "$NORMAL_TOKEN" 1 &
  wait

  filler_count="$(grep -c '^filler-' "$tmpdir/results" || true)"
  printf '%-12s %-9s %s\n' "label" "http_code" "time_total"
  grep -v '^filler-' "$tmpdir/results" | sort | while read -r label code time_total; do
    printf '%-12s %-9s %s\n' "$label" "$code" "$time_total"
  done
  echo "  (+ $filler_count saturation filler requests fired this round)"

  # Surface response bodies for anything unexpected (not 200, not 503) on the
  # actual probes (skip fillers — noisy, and we only care about the probes).
  while read -r label code _; do
    case "$label" in filler-*) continue ;; esac
    if [[ "$code" != "200" && "$code" != "503" ]]; then
      body_file="$(ls "$tmpdir/body_${label}_"* 2>/dev/null | head -1)"
      echo "    --- $label got unexpected $code: $(head -c 300 "$body_file" 2>/dev/null) ---"
    fi
  done < "$tmpdir/results"

  round_priority_fail=0
  while read -r label code _; do
    case "$label" in
      priority-*)
        priority_total=$((priority_total + 1))
        if [[ "$code" == "200" ]]; then
          priority_ok=$((priority_ok + 1))
        else
          round_priority_fail=1
        fi
        ;;
    esac
  done < "$tmpdir/results"
  if [[ "$round_priority_fail" == "1" ]]; then
    echo "  FAIL: at least one priority-token probe did not get 200 this round."
    overall_pass=0
  fi

  # Any non-200 counts as "rejected" here (usually 503, but a 429 from some
  # other layer — e.g. an upstream rate limiter — is just as valid evidence
  # that the pool was under real contention this round).
  normal_rejected=0
  while read -r label code _; do
    [[ "$label" == normal-* && "$code" != "200" ]] && normal_rejected=1
  done < "$tmpdir/results"
  spoof_code="$(awk '$1=="spoof"{print $2}' "$tmpdir/results")"
  if [[ "$normal_rejected" == "1" && "$spoof_code" == "200" ]]; then
    echo "  FAIL: spoofed ${SPOOF_HEADER} header let a normal token through (200) while a genuine normal request was rejected (503) under the same contention — the header is not being stripped, priority can be self-granted!"
    overall_pass=0
  fi
  echo
done

echo "==> priority-token probes: ${priority_ok}/${priority_total} succeeded across ${ROUNDS} round(s)"
if [[ "$overall_pass" == "1" && "$priority_total" -gt 0 && "$priority_ok" == "$priority_total" ]]; then
  echo "PASS: every priority-token request was served in priority, and no spoofed header ever bypassed the shared pool."
  exit 0
fi
echo "FAIL: see above."
exit 1
