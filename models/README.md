# models — liberated model recipes

Reproducible builds for the models cloister serves.  A "liberated" model is a
stock base with its refusal behavior removed by
[Heretic](https://github.com/p-e-w/heretic) (automatic directional ablation),
converted to GGUF, and registered with ollama into the host model store the
jailed [`infer`](../docker/inference.yaml) container mounts read-only.

The rationale — why we build these instead of downloading third-party
abliterations, the AGPL and trust boundaries, and the endgame of serving *only*
liberated models — is in [docs/heretic.md](../docs/heretic.md).  This file is
the operator's how-to.

> **This runs off the jail.**  A build pulls the base model and heretic's
> calibration datasets from Hugging Face over the open internet.  That is the
> same trust boundary as `ollama pull` staging — run it on the host/build box,
> **never** inside `infer` (which has no egress by design).

## A recipe

One directory per model:

```
<recipe>/
  recipe.conf    what to build   — BASE_MODEL, QUANT, OLLAMA_TAG, optional BASE_COMMIT
  heretic.toml   how to ablate   — heretic ablation profile (datasets, trials, quantization)
  Modelfile      how to register — ollama params/template; FROM is @@GGUF@@ (filled in at build)
```

Shipped recipes:

- **`qwen3-4b-smoke/`** — the fastest end-to-end run (~20-30 min on a 24 GB
  card).  Proves the toolchain before you commit hours to a big model.  Not
  meant to be served.
- **`qwen3-coder-30b/`** — the pilot: liberate stock
  `Qwen/Qwen3-Coder-30B-A3B-Instruct` ourselves and A/B it against the
  downloaded `huihui_ai/qwen3-coder-abliterated:30b`.

## Prerequisites

- **heretic** — `pip install -U heretic-llm`; needs a CUDA GPU.  A 24 GB card
  (3090/4090) handles a 4B unquantized and a 30B via `bnb_4bit` (set in the
  recipe's `heretic.toml`).
- **llama.cpp** checkout — provides `convert_hf_to_gguf.py` and a built
  `llama-quantize`.
- **ollama** — for `ollama create`.

## Build

```sh
# smoke-test the toolchain first
LLAMA_CPP=~/src/llama.cpp MODELS_DIR=c:/ai_models \
  models/liberate.sh models/qwen3-4b-smoke

# then the real pilot
LLAMA_CPP=~/src/llama.cpp MODELS_DIR=c:/ai_models \
  models/liberate.sh models/qwen3-coder-30b
```

`liberate.sh <recipe-dir>` runs the whole pipeline and registers the tag into
`MODELS_DIR` (default `c:/ai_models`, matching `docker/inference.yaml`).  Key env
vars — `MODELS_DIR`, `LLAMA_CPP`, `LLAMA_QUANTIZE`, `WORK_ROOT`,
`KEEP_INTERMEDIATES` — are documented in the script header.

During the run heretic prints the selected trial's **refusal rate** and **KL
divergence**; record them.  But those are measured on heretic's in-memory
PyTorch model, **before** GGUF conversion and quantization — so they qualify the
*abliteration*, not the *artifact we serve*.  Each run also drops a `build-info`
stamp and heretic's `reproduce.json` in the work dir.

## Qualify the converted model

Quantization is lossy and can partially **restore** refusals, so the served tag
needs its own check.  Two metrics, splitting the two costs:

**1. Refusals on the served tag** (the one quantization most threatens).
`qualify.py` re-runs heretic's exact `KeywordRate` scorer against the running
ollama endpoint, so the number is directly comparable to heretic's pre-quant
one.  It imports heretic's matcher from the installed package (never vendored —
heretic is AGPL, and stays a build-time tool), so run it in the heretic env:

```sh
# with the inference stack (or a host ollama) serving the tag:
python3 models/qualify.py --model qwen3-coder-liberated:30b
python3 models/qualify.py --model qwen3-coder-liberated:30b --print-responses   # eyeball
```

A large gap from heretic's number means quantization undid some liberation — try
a higher-fidelity quant (`Q5_K_M`/`Q6_K`) in the recipe.

**2. Quantization KL vs. the f16 baseline** (the cost of conversion itself).
Faithful KL needs full next-token distributions, which ollama does not expose,
so use the canonical llama.cpp tool against the GGUFs directly.  `liberate.sh`
keeps the f16 baseline (`model-f16.gguf` in the work dir) for exactly this:

```sh
llama-perplexity -m <work>/model-f16.gguf     -f corpus.txt --kl-divergence-base <work>/kl.base
llama-perplexity -m <work>/model-Q4_K_M.gguf  -f corpus.txt --kl-divergence --kl-divergence-base <work>/kl.base
```

(`corpus.txt` is any representative text sample, e.g. wikitext.)  Heretic's KL is
the cost of liberation; this KL is the cost of quantization; together they
bracket the served model end to end.

## Serve

```sh
OLLAMA_MODELS=c:/ai_models ollama show qwen3-coder-liberated:30b
```

Point a cell at it by setting `OPENAI_MODEL=qwen3-coder-liberated:30b` (see
[docs/GETTING_STARTED.md](../docs/GETTING_STARTED.md)).  No compose change: the
tag is already in the store `infer` mounts.
