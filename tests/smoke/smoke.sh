#!/usr/bin/env bash
# tests/smoke/smoke.sh — Smoke tests for the GatewAI gateway (dev / staging)
#
# Usage:
#   GATEWAY_URL=https://api.example.com \
#   AUDIO_URL=https://... \
#   bash tests/smoke/smoke.sh
#
# Required env vars:
#   GATEWAY_URL       Base URL of the gateway (no trailing slash)
#   AUDIO_URL         URL of a short speech audio file (.mp3 / .wav / .m4a)
#
# Optional env vars:
#   WHISPER_API_KEY   Zitadel Bearer token for audio endpoints (sent as 'Authorization: Bearer' header)
#   RERANK_API_KEY    Zitadel Bearer token for rerank endpoint  (sent as 'Authorization: Bearer' header)
#   WHISPER_MODEL     Model name for Whisper (default: whisper-large-v3)
#   RERANK_MODEL      Model name for the reranker (default: bge-reranker-v2-m3)
#   RERANK_ENDPOINT   Rerank path (default: /rerank)
#   POLL_TIMEOUT      Max seconds to wait for an async job (default: 300)
#   POLL_INTERVAL     Seconds between async polls (default: 5)
#   SYNC_TIMEOUT      Max seconds for the synchronous transcription request (default: 300)
#   SKIP_WHISPER      Set to 1 to skip both Whisper tests
#   SKIP_RERANK       Set to 1 to skip the Rerank test

set -euo pipefail

# ── Configuration ──────────────────────────────────────────────────────────────
GATEWAY_URL="${GATEWAY_URL:?GATEWAY_URL is required (e.g. https://api.gatewai.example.com)}"
AUDIO_URL="${AUDIO_URL:?AUDIO_URL is required (URL of a short .mp3 / .wav speech file)}"
WHISPER_API_KEY="${WHISPER_API_KEY:-}"
RERANK_API_KEY="${RERANK_API_KEY:-}"
WHISPER_MODEL="${WHISPER_MODEL:-whisper-large-v3}"
RERANK_MODEL="${RERANK_MODEL:-bge-reranker-v2-m3}"
RERANK_ENDPOINT="${RERANK_ENDPOINT:-/rerank}"
POLL_TIMEOUT="${POLL_TIMEOUT:-300}"
POLL_INTERVAL="${POLL_INTERVAL:-5}"
SYNC_TIMEOUT="${SYNC_TIMEOUT:-300}"
SKIP_WHISPER="${SKIP_WHISPER:-0}"
SKIP_RERANK="${SKIP_RERANK:-0}"

# ── State ──────────────────────────────────────────────────────────────────────
PASS=0
FAIL=0
SKIP=0
declare -a ERRORS=()

# ── Helpers ────────────────────────────────────────────────────────────────────
GREEN='\033[0;32m'; RED='\033[0;31m'; YELLOW='\033[0;33m'; BOLD='\033[1m'; NC='\033[0m'

log()  { echo "[$(date -u '+%H:%M:%S')] $*"; }
pass() { PASS=$((PASS+1));  printf "  ${GREEN}PASS${NC} %s\n" "$1"; }
fail() { FAIL=$((FAIL+1));  printf "  ${RED}FAIL${NC} %s\n" "$1"; ERRORS+=("$1"); }
skip() { SKIP=$((SKIP+1));  printf "  ${YELLOW}SKIP${NC} %s\n" "$1"; }

# Per-service curl base args (auth header differs between Whisper and Rerank).
CURL_BASE=(-s --max-time 30 -H "Accept: application/json")

curl_whisper() {
  local args=("${CURL_BASE[@]}")
  [[ -n "$WHISPER_API_KEY" ]] && args+=(-H "Authorization: Bearer ${WHISPER_API_KEY}")
  curl "${args[@]}" "$@"
}

curl_rerank() {
  local args=("${CURL_BASE[@]}")
  [[ -n "$RERANK_API_KEY" ]] && args+=(-H "Authorization: Bearer ${RERANK_API_KEY}")
  curl "${args[@]}" "$@"
}

# Health check has no auth requirement — use bare base args.
http_health_status() {
  curl "${CURL_BASE[@]}" -o /dev/null -w "%{http_code}" \
    "${GATEWAY_URL}/health" 2>/dev/null || echo "000"
}

# ── Download audio file once ───────────────────────────────────────────────────
AUDIO_FILE=""
if [[ "$SKIP_WHISPER" != "1" ]]; then
  AUDIO_FILE=$(mktemp --suffix=".mp3")
  trap 'rm -f "${AUDIO_FILE:-}"' EXIT

  log "Downloading audio from ${AUDIO_URL} ..."
  if ! curl -fsSL --max-time 60 "$AUDIO_URL" -o "$AUDIO_FILE"; then
    echo "ERROR: failed to download audio file from ${AUDIO_URL}" >&2
    exit 2
  fi
  AUDIO_SIZE=$(stat -c%s "$AUDIO_FILE")
  log "Audio ready: ${AUDIO_SIZE} bytes"
fi

# ══════════════════════════════════════════════════════════════════════════════
# 1 / 4  Health check
# ══════════════════════════════════════════════════════════════════════════════
log ""
log "=== 1/4  Health check ==="

STATUS=$(http_health_status)
if [[ "$STATUS" == "200" ]]; then
  pass "GET /health → 200"
else
  fail "GET /health → ${STATUS} (expected 200)"
fi

# ══════════════════════════════════════════════════════════════════════════════
# 2 / 4  Whisper — async (POST /jobs/audio)
# ══════════════════════════════════════════════════════════════════════════════
log ""
log "=== 2/4  Whisper async (POST /jobs/audio) ==="

if [[ "$SKIP_WHISPER" == "1" ]]; then
  skip "SKIP_WHISPER=1"
else
  SUBMIT_BODY=$(curl_whisper \
    -X POST \
    -F "file=@${AUDIO_FILE};type=audio/mpeg" \
    -F "model=${WHISPER_MODEL}" \
    -F "operation=transcription" \
    -w "\n__HTTP_STATUS__:%{http_code}" \
    "${GATEWAY_URL}/jobs/audio" 2>&1) || true
  _HTTP=$(echo "$SUBMIT_BODY" | grep -o '__HTTP_STATUS__:[0-9]*' | cut -d: -f2 || true)
  SUBMIT_BODY=$(echo "$SUBMIT_BODY" | sed 's/__HTTP_STATUS__:[0-9]*//')
  [[ -z "$_HTTP" ]] && _HTTP="000"

  JOB_ID=$(echo "$SUBMIT_BODY" | jq -r '.job_id // empty' 2>/dev/null || true)

  if [[ -z "$JOB_ID" || "$JOB_ID" == "null" ]]; then
    fail "POST /jobs/audio → HTTP ${_HTTP} — no job_id in response: ${SUBMIT_BODY}"
  else
    pass "POST /jobs/audio → job_id=${JOB_ID}"
    log "  Polling GET /jobs/audio/${JOB_ID} (timeout=${POLL_TIMEOUT}s) ..."

    ELAPSED=0
    JOB_STATUS=""
    while [[ $ELAPSED -lt $POLL_TIMEOUT ]]; do
      POLL_BODY=$(curl_whisper "${GATEWAY_URL}/jobs/audio/${JOB_ID}" 2>&1) || POLL_BODY=""
      JOB_STATUS=$(echo "$POLL_BODY" | jq -r '.status // empty' 2>/dev/null || true)

      case "$JOB_STATUS" in
        completed)
          RESULT_LEN=$(echo "$POLL_BODY" | jq -r '.result | tostring | length' 2>/dev/null || echo "0")
          pass "Async job completed (result: ${RESULT_LEN} chars)"
          break
          ;;
        failed)
          JOB_ERR=$(echo "$POLL_BODY" | jq -r '.error // "(no error field)"' 2>/dev/null || true)
          fail "Async job failed: ${JOB_ERR}"
          break
          ;;
        pending|processing)
          log "  ... status=${JOB_STATUS} (${ELAPSED}s elapsed)"
          ;;
        *)
          log "  ... unexpected status=${JOB_STATUS:-<empty>} (${ELAPSED}s elapsed)"
          ;;
      esac

      sleep "$POLL_INTERVAL"
      ELAPSED=$((ELAPSED + POLL_INTERVAL))
    done

    if [[ $ELAPSED -ge $POLL_TIMEOUT && "$JOB_STATUS" != "completed" && "$JOB_STATUS" != "failed" ]]; then
      fail "Async job timed out after ${POLL_TIMEOUT}s (last status: ${JOB_STATUS:-unknown})"
    fi
  fi
fi

# ══════════════════════════════════════════════════════════════════════════════
# 3 / 4  Whisper — sync (POST /v1/audio/transcriptions)
# ══════════════════════════════════════════════════════════════════════════════
log ""
log "=== 3/4  Whisper sync (POST /v1/audio/transcriptions) ==="

if [[ "$SKIP_WHISPER" == "1" ]]; then
  skip "SKIP_WHISPER=1"
else
  SYNC_BODY=$(curl_whisper \
    --max-time "$SYNC_TIMEOUT" \
    -X POST \
    -F "file=@${AUDIO_FILE};type=audio/mpeg" \
    -F "model=${WHISPER_MODEL}" \
    -w "\n__HTTP_STATUS__:%{http_code}" \
    "${GATEWAY_URL}/v1/audio/transcriptions" 2>&1) || true
  _SYNC_HTTP=$(echo "$SYNC_BODY" | grep -o '__HTTP_STATUS__:[0-9]*' | cut -d: -f2 || true)
  SYNC_BODY=$(echo "$SYNC_BODY" | sed 's/__HTTP_STATUS__:[0-9]*//')

  TRANSCRIPTION=$(echo "$SYNC_BODY" | jq -r '.text // empty' 2>/dev/null || true)

  if [[ -n "$TRANSCRIPTION" && "$TRANSCRIPTION" != "null" ]]; then
    TRUNC="${TRANSCRIPTION:0:80}"
    pass "Sync transcription → \"${TRUNC}...\" (${#TRANSCRIPTION} chars)"
  else
    fail "Sync transcription → HTTP ${_SYNC_HTTP:-000} — no .text in response: ${SYNC_BODY}"
  fi
fi

# ══════════════════════════════════════════════════════════════════════════════
# 4 / 4  Rerank — sync-direct (POST /rerank)
# ══════════════════════════════════════════════════════════════════════════════
log ""
log "=== 4/4  Rerank sync-direct (POST ${RERANK_ENDPOINT}) ==="

if [[ "$SKIP_RERANK" == "1" ]]; then
  skip "SKIP_RERANK=1"
else
  RERANK_PAYLOAD=$(jq -n \
    --arg q   "What is retrieval augmented generation?" \
    --arg m   "$RERANK_MODEL" \
    --argjson docs '[
      "Paris is the capital and most populous city of France.",
      "RAG combines retrieval systems with generative models to ground responses in facts.",
      "Vector databases store data as high-dimensional vectors for efficient similarity search.",
      "The transformer architecture uses self-attention mechanisms to process sequences.",
      "Fine-tuning adapts a pre-trained model to a specific task using additional training data.",
      "BERT is a bidirectional encoder while GPT is a unidirectional autoregressive model.",
      "Reranker evaluation uses metrics like NDCG, MRR, and precision@k.",
      "Semantic search finds documents based on meaning rather than keyword matching."
    ]' \
    '{query: $q, model: $m, texts: $docs, top_n: 3, return_documents: true}')

  RERANK_HTTP_STATUS=""
  RERANK_BODY=$(curl_rerank \
    -X POST \
    -H "Content-Type: application/json" \
    -d "$RERANK_PAYLOAD" \
    -w "\n__HTTP_STATUS__:%{http_code}" \
    "${GATEWAY_URL}${RERANK_ENDPOINT}" 2>&1) || true
  # Extract HTTP status appended by -w and strip it from body
  RERANK_HTTP_STATUS=$(echo "$RERANK_BODY" | grep -o '__HTTP_STATUS__:[0-9]*' | cut -d: -f2 || true)
  RERANK_BODY=$(echo "$RERANK_BODY" | sed 's/__HTTP_STATUS__:[0-9]*//')

  # Accept bare array, {"results":[...]}, or {"data":[...]}
  RESULTS_COUNT=$(echo "$RERANK_BODY" | \
    jq 'if type == "array" then length else (.results // .data // [] | length) end' \
    2>/dev/null || echo "0")

  if [[ "$RESULTS_COUNT" -gt 0 ]]; then
    TOP_IDX=$(echo "$RERANK_BODY" | jq -r '(.results // .data // .)[0].index // "?"' 2>/dev/null || true)
    pass "Rerank → ${RESULTS_COUNT} results (top index=${TOP_IDX})"
  else
    fail "Rerank → HTTP ${RERANK_HTTP_STATUS:-000} — no results in response: ${RERANK_BODY}"
  fi
fi

# ══════════════════════════════════════════════════════════════════════════════
# Summary
# ══════════════════════════════════════════════════════════════════════════════
log ""
printf "${BOLD}══════════════════════════════════${NC}\n"
printf "  ${GREEN}%d passed${NC}  ${RED}%d failed${NC}  ${YELLOW}%d skipped${NC}\n" "$PASS" "$FAIL" "$SKIP"
printf "${BOLD}══════════════════════════════════${NC}\n"

if [[ ${#ERRORS[@]} -gt 0 ]]; then
  echo ""
  echo "Failed:"
  for e in "${ERRORS[@]}"; do
    printf "  ${RED}✗${NC} %s\n" "$e"
  done
  exit 1
fi
exit 0
