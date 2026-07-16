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

# liberate.sh — build one liberated model from a recipe directory.
#
# Pipeline (see docs/heretic.md):
#   1. heretic                    base safetensors -> abliterated safetensors
#   2. convert_hf_to_gguf.py      safetensors      -> GGUF f16
#   3. llama-quantize             GGUF f16         -> GGUF <QUANT>
#   4. ollama create              GGUF + Modelfile -> tag in $MODELS_DIR
#
# This is a HOST-SIDE build, run off the jail: steps 1-2 pull the base and
# heretic's calibration datasets from Hugging Face over the open internet,
# exactly the trust boundary of `ollama pull` staging. Never run it inside the
# jailed `infer` container. The tag lands in the same read-only store `infer`
# mounts, so a rebuild is picked up on the next model load — no compose change.
#
# Usage:
#   models/liberate.sh <recipe-dir>
#   models/liberate.sh models/qwen3-4b-smoke      # validate the toolchain first
#   models/liberate.sh models/qwen3-coder-30b     # the real build
#
# Prerequisites on PATH (or via the env vars below):
#   - heretic            (`pip install -U heretic-llm`; needs a CUDA GPU)
#   - python3 + a llama.cpp checkout (convert_hf_to_gguf.py + llama-quantize)
#   - ollama
#
# Environment (all optional; sensible defaults):
#   MODELS_DIR       ollama store the build writes into (default: c:/ai_models,
#                    matching docker/inference.yaml's default mount source).
#   LLAMA_CPP        llama.cpp checkout holding convert_hf_to_gguf.py.
#   LLAMA_QUANTIZE   llama-quantize binary (default: $LLAMA_CPP/build/bin/llama-quantize).
#   WORK_ROOT        scratch root for intermediates (default: $TMPDIR/liberate).
#   KEEP_INTERMEDIATES  set non-empty to keep the safetensors + f16 GGUF.

set -euo pipefail

die() { echo "liberate: $*" >&2; exit 1; }
step() { echo; echo "==> $*"; }

# ---- Arguments and recipe ---------------------------------------------------

[ $# -eq 1 ] || die "usage: $0 <recipe-dir>"
RECIPE_DIR=$(cd "$1" 2>/dev/null && pwd) || die "no such recipe directory: $1"
RECIPE_CONF="$RECIPE_DIR/recipe.conf"
HERETIC_CONFIG="$RECIPE_DIR/heretic.toml"
MODELFILE_TEMPLATE="$RECIPE_DIR/Modelfile"

for f in "$RECIPE_CONF" "$HERETIC_CONFIG" "$MODELFILE_TEMPLATE"; do
  [ -f "$f" ] || die "recipe is missing $(basename "$f") in $RECIPE_DIR"
done

# recipe.conf supplies: BASE_MODEL, OLLAMA_TAG, QUANT, and optional BASE_COMMIT.
BASE_COMMIT=""
# shellcheck disable=SC1090
source "$RECIPE_CONF"
: "${BASE_MODEL:?recipe.conf must set BASE_MODEL (a Hugging Face model id)}"
: "${OLLAMA_TAG:?recipe.conf must set OLLAMA_TAG (the ollama tag to create)}"
: "${QUANT:?recipe.conf must set QUANT (a llama-quantize type, e.g. Q4_K_M)}"

# ---- Environment defaults ---------------------------------------------------

MODELS_DIR="${MODELS_DIR:-c:/ai_models}"
LLAMA_CPP="${LLAMA_CPP:-}"
LLAMA_QUANTIZE="${LLAMA_QUANTIZE:-${LLAMA_CPP:+$LLAMA_CPP/build/bin/llama-quantize}}"
WORK_ROOT="${WORK_ROOT:-${TMPDIR:-/tmp}/liberate}"

[ -n "$LLAMA_CPP" ] || die "set LLAMA_CPP to your llama.cpp checkout (needs convert_hf_to_gguf.py)"
CONVERT="$LLAMA_CPP/convert_hf_to_gguf.py"
[ -f "$CONVERT" ] || die "convert script not found: $CONVERT"
[ -n "$LLAMA_QUANTIZE" ] && [ -x "$LLAMA_QUANTIZE" ] || die "llama-quantize not found/executable: ${LLAMA_QUANTIZE:-<unset>}"
command -v heretic >/dev/null || die "heretic not on PATH (pip install -U heretic-llm)"
command -v ollama  >/dev/null || die "ollama not on PATH"

SLUG=$(basename "$RECIPE_DIR")
WORK="$WORK_ROOT/$SLUG"
SAFETENSORS_DIR="$WORK/safetensors"
GGUF_F16="$WORK/model-f16.gguf"
GGUF_Q="$WORK/model-$QUANT.gguf"
RENDERED_MODELFILE="$WORK/Modelfile"
mkdir -p "$WORK"

echo "liberate: recipe   = $RECIPE_DIR"
echo "liberate: base     = $BASE_MODEL${BASE_COMMIT:+ @ $BASE_COMMIT}"
echo "liberate: quant    = $QUANT"
echo "liberate: tag      = $OLLAMA_TAG  ->  $MODELS_DIR"
echo "liberate: work dir = $WORK"

# ---- 1. Abliterate ----------------------------------------------------------
# heretic reads config.toml from its working directory (pydantic-settings). We
# run inside $WORK with the recipe's heretic.toml copied in as config.toml, and
# pass the run-specific bits as flags so the recipe stays a reusable profile.
# --model-action save + --save-directory make the run fully non-interactive.
step "1/4 abliterating $BASE_MODEL with heretic"
cp "$HERETIC_CONFIG" "$WORK/config.toml"
rm -rf "$SAFETENSORS_DIR"
( cd "$WORK" && heretic \
    --model "$BASE_MODEL" \
    ${BASE_COMMIT:+--model-commit "$BASE_COMMIT"} \
    --save-directory "$SAFETENSORS_DIR" \
    --model-action save )
[ -f "$SAFETENSORS_DIR/config.json" ] || die "heretic did not produce a model in $SAFETENSORS_DIR"
# Keep the reproducibility record with the build.
[ -f "$SAFETENSORS_DIR/reproduce.json" ] && cp "$SAFETENSORS_DIR/reproduce.json" "$WORK/reproduce.json" || true

# ---- 2. Convert to GGUF -----------------------------------------------------
step "2/4 converting safetensors -> GGUF f16"
python3 "$CONVERT" "$SAFETENSORS_DIR" --outfile "$GGUF_F16" --outtype f16

# ---- 3. Quantize ------------------------------------------------------------
step "3/4 quantizing -> $QUANT"
"$LLAMA_QUANTIZE" "$GGUF_F16" "$GGUF_Q" "$QUANT"

# ---- 4. Register with ollama ------------------------------------------------
# The recipe Modelfile carries params/template but leaves the source as the
# placeholder @@GGUF@@; substitute the quantized artifact's absolute path.
step "4/4 registering $OLLAMA_TAG in $MODELS_DIR"
sed "s|@@GGUF@@|$GGUF_Q|" "$MODELFILE_TEMPLATE" > "$RENDERED_MODELFILE"
OLLAMA_MODELS="$MODELS_DIR" ollama create "$OLLAMA_TAG" -f "$RENDERED_MODELFILE"

# ---- Provenance stamp -------------------------------------------------------
cat > "$WORK/build-info" <<EOF
tag=$OLLAMA_TAG
base=$BASE_MODEL
base_commit=${BASE_COMMIT:-unpinned}
quant=$QUANT
recipe=$SLUG
built_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
models_dir=$MODELS_DIR
EOF

# The f16 GGUF is the baseline for the quantization-KL check (see below), so it
# is kept by default; the raw safetensors is large and only needed to re-quant.
if [ -z "${KEEP_INTERMEDIATES:-}" ]; then
  rm -rf "$SAFETENSORS_DIR"
fi

echo
echo "liberate: done. $OLLAMA_TAG is staged in $MODELS_DIR."
echo "liberate: provenance  -> $WORK/build-info  (and reproduce.json if heretic wrote one)"
echo "liberate: verify      -> OLLAMA_MODELS=$MODELS_DIR ollama show $OLLAMA_TAG"
echo
echo "liberate: heretic printed the SELECTED TRIAL's refusal rate + KL above (pre-quant). Record them."
echo "liberate: qualify the CONVERTED model (quantization can restore refusals):"
echo "liberate:   1. refusals on the served tag:"
echo "liberate:        python3 models/qualify.py --model $OLLAMA_TAG"
echo "liberate:   2. quantization KL vs the f16 baseline (canonical llama.cpp tool):"
echo "liberate:        llama-perplexity -m $GGUF_F16 -f <corpus.txt> --kl-divergence-base $WORK/kl.base"
echo "liberate:        llama-perplexity -m $GGUF_Q   -f <corpus.txt> --kl-divergence --kl-divergence-base $WORK/kl.base"
echo "liberate: (f16 baseline kept at $GGUF_F16; delete it when qualified.)"
