#!/usr/bin/env bash
# Copyright 2026 Jeffrey B. Stewart
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# deepthink-serve.sh -- launch the jailed ollama deep-think node.
#
# Runs `ollama serve` inside a macOS seatbelt sandbox (no network egress,
# model store read-only, home dir invisible), wrapped in `caffeinate -s`
# so the Mac will not sleep while serving on AC, with a blind socat relay
# exposing it to the LAN.  Foreground: it tails the logs to this console
# and cleans everything up on Ctrl-C.
#
# Layers present: seatbelt (layer 1) + relay (layer 4) + no-sleep.
# Still TODO for full deepthink.md parity: PF per-uid backstop (layer 2),
# egress probe (layer 3), dedicated service user, launchd.  Those need
# sudo and come next.
set -euo pipefail

# ---- config -----------------------------------------------------------
ROOT=/opt/deepthink
PROFILE="$ROOT/ollama.sb"
OLLAMA=/opt/homebrew/bin/ollama
LOOPBACK_PORT=11435          # ollama binds loopback here (internal)
LAN_PORT=11434               # socat exposes this on all interfaces
LOGDIR="$ROOT/logs"
SERVE_LOG="$LOGDIR/serve.log"
RELAY_LOG="$LOGDIR/relay.log"

# ---- preflight --------------------------------------------------------
for f in "$PROFILE" "$OLLAMA"; do
  [ -e "$f" ] || { echo "FATAL: missing $f" >&2; exit 1; }
done
command -v socat >/dev/null || { echo "FATAL: socat not installed (brew install socat)" >&2; exit 1; }
mkdir -p "$ROOT/run" "$ROOT/tmp" "$ROOT/cache" "$LOGDIR"

# Stop anything already holding the ports (a prior run, or the Gate 1
# native serve).  Idempotent.
pkill -f "sandbox-exec -f $PROFILE" 2>/dev/null || true
pkill -f "TCP-LISTEN:$LAN_PORT"     2>/dev/null || true
pkill -f "libexec/lib/ollama/llama-server" 2>/dev/null || true
sleep 1

cleanup() {
  echo; echo "[deepthink] shutting down..."
  pkill -f "TCP-LISTEN:$LAN_PORT"           2>/dev/null || true
  pkill -f "sandbox-exec -f $PROFILE"       2>/dev/null || true
  pkill -f "libexec/lib/ollama/llama-server" 2>/dev/null || true
  # caffeinate exits once its wrapped command does; nudge it just in case.
  pkill -f "caffeinate -s sandbox-exec -f $PROFILE" 2>/dev/null || true
  echo "[deepthink] stopped.  The Mac can sleep again."
}
trap cleanup EXIT INT TERM

# ---- jailed serve -----------------------------------------------------
# cwd MUST be inside the sandbox-readable tree: llama-server calls getcwd()
# at startup, and the sandbox hides the operator's home, so launching from
# ~ makes it abort ("getcwd failed").  /opt/deepthink/run is allowed.
echo "[deepthink] starting jailed ollama serve (loopback :$LOOPBACK_PORT)..."
: > "$SERVE_LOG"
# Two lanes, resident and pinned: MAX_LOADED_MODELS=2 holds both models,
# KEEP_ALIVE=-1 never idle-unloads them, NUM_PARALLEL=1 serializes each.
# Global "one request at a time across BOTH models" is enforced upstream by
# the agency's per-node maxInFlight:1 admission gate (cloister
# internal/agency) -- ollama has no cross-model concurrency knob of its own.
(
  cd "$ROOT/run"
  OLLAMA_MODELS="$ROOT/models" \
  OLLAMA_HOST="127.0.0.1:$LOOPBACK_PORT" \
  HOME="$ROOT/run" \
  TMPDIR="$ROOT/tmp" \
  OLLAMA_FLASH_ATTENTION=1 \
  OLLAMA_KV_CACHE_TYPE=q8_0 \
  OLLAMA_MAX_LOADED_MODELS=2 \
  OLLAMA_KEEP_ALIVE=-1 \
  OLLAMA_NUM_PARALLEL=1 \
  exec caffeinate -s sandbox-exec -f "$PROFILE" "$OLLAMA" serve
) >>"$SERVE_LOG" 2>&1 &

# Wait for the API to answer on loopback.
for i in $(seq 1 60); do
  curl -sS -m 2 "http://127.0.0.1:$LOOPBACK_PORT/api/version" >/dev/null 2>&1 && break
  pgrep -f "sandbox-exec -f $PROFILE" >/dev/null || { echo "FATAL: serve died -- see $SERVE_LOG" >&2; tail -20 "$SERVE_LOG" >&2; exit 1; }
  sleep 1
done
echo "[deepthink] ollama up.  (GPU discovery takes a few seconds.)"

# ---- blind LAN relay --------------------------------------------------
: > "$RELAY_LOG"
socat TCP-LISTEN:"$LAN_PORT",reuseaddr,fork TCP:127.0.0.1:"$LOOPBACK_PORT" >>"$RELAY_LOG" 2>&1 &
sleep 1
lan_ip=$(ipconfig getifaddr en0 2>/dev/null || ipconfig getifaddr en1 2>/dev/null || echo '<lan-ip>')
echo "[deepthink] relay up.  Reachable on the LAN at:  http://$lan_ip:$LAN_PORT"
echo "[deepthink] tailing logs (Ctrl-C to stop the node)..."
echo "-----------------------------------------------------------------------"

# ---- tail to console --------------------------------------------------
# -F follows across truncation/rotation; both logs, each block labelled.
tail -n +1 -F "$SERVE_LOG" "$RELAY_LOG"
