# The heretic bench — liberated model builds design

Status: **design + toolchain drafted; pilot pending.**  Decisions from the
2026-07-12 investigation.  This is the build-side counterpart to the
[agency](agency.md) and the [deep-think node](deepthink.md): where those own
*serving* inference, the heretic bench owns *producing* the weights they serve.
Not a running service — an offline recipe tree and a build script under
[`models/`](../models).

## Problem

Cloister is a containment-first environment for real engineering work,
explicitly including authorized security testing (see the top of
[CLAUDE.md](../CLAUDE.md)).  Stock instruction-tuned models refuse a slice of
exactly that work — a legitimate exploit-development task, a systems-level
"how do I disable X", a security review that must reason about attack code —
with "I can't help with that."  In an autonomous pipeline that refusal is not
a polite decline to a human; it is a **silent failure of a build, review, or
comprehension step**, injected by the model's alignment training, invisible to
the audit trail because nothing errored.

We already work around this, badly.  The host model store
(`c:/ai_models`, the `MODELS_DIR` the [`infer`](../docker/inference.yaml)
container mounts read-only) today holds *downloaded* third-party abliterations
— `huihui_ai/qwen3-coder-abliterated`, `huihui_ai/qwen2.5-coder-abliterate` —
with a thin local `ollama create` on top for a system prompt (`qwen3-free`) and
parameter overrides (`qwen3-coder-abliterated-gpu-unbound`).  Two problems:

1. **Provenance.**  We trust an opaque upload.  We cannot say what base it came
   from, what was ablated, or at what cost to capability — in the one place
   cloister is otherwise fanatical about provenance and one-way audit.  It is a
   supply-chain surface we would reject anywhere else in the tree.
2. **Reach and quality.**  We get only what that uploader publishes, tuned by
   hand, at whatever capability damage their manual process incurs.

## What heretic is

[Heretic](https://github.com/p-e-w/heretic) (`p-e-w/heretic`, AGPL-3.0) removes
refusal behavior from a transformer **without retraining**.  It implements
*directional ablation* ("abliteration") — orthogonalizing each layer's weights
against a "refusal direction" derived from the difference of mean hidden states
on harmful vs. harmless prompts — and wraps it in an Optuna TPE optimizer that
**co-minimizes two measured objectives**: refusal count on harmful prompts
(`KeywordRate`) and KL divergence from the original model on harmless prompts.
The result is unsupervised yet competitive with hand-tuned abliterations: on
Gemma-3-12B, heretic reached the same 3/100 refusal rate as the manual
`mlabonne` and `huihui-ai` versions at **KL 0.16 vs. their 1.04 / 0.45** — i.e.
the same uncensoring at materially less damage to the model.

It runs fully non-interactive (pydantic-settings: a `config.toml` in the working
directory, overridable by `--kebab-case` flags and `HERETIC_*` env vars; set
`model_action = "save"` + `save_directory` and no prompt appears).  It emits a
**Hugging Face safetensors** model and a `reproduce.json` reproducibility
record.

## Endgame: liberation as policy, not an option

The intent is that **liberated models become the only models cloister serves.**
A stock model that can refuse a legitimate action is a latent fault in every
autonomous lane — builder, librarian comprehension, corrector review.  Making
liberation uniform removes that fault class *and* turns model identity into a
first-class, in-repo, reproducible artifact instead of a downloaded black box.
The pilot below is the gate; if it clears, the downloaded `*-abliterated` tags
retire and the "no stock models" stance gets written into GETTING_STARTED.

## The one real gap: format

Heretic emits safetensors; `infer` (ollama) consumes GGUF.  So a liberated build
is a short pipeline, and its last two steps are exactly the manual `ollama
create` we already run:

| # | Step | Tool | Output |
|---|------|------|--------|
| 1 | Abliterate | `heretic` (GPU) | HF safetensors + `reproduce.json` |
| 2 | Convert | `llama.cpp/convert_hf_to_gguf.py` | GGUF f16 |
| 3 | Quantize | `llama-quantize` | GGUF `Q4_K_M` (VRAM-fit) |
| 4 | Register | `ollama create -f Modelfile` | tag in `MODELS_DIR` |
| 5 | Serve | mount `:ro` into `infer` | no repo change |

Steps 1–3 are new; steps 4–5 are unchanged from today's flow.
[`models/liberate.sh`](../models/liberate.sh) drives 1–4 from a recipe.

**Qualifying the served artifact.**  Heretic measures refusals and KL on its
in-memory PyTorch model — step 1's output, *before* quantization.  Quantization
(step 3) is lossy and can partially restore refusals, so the tag we actually
serve gets its own qualification, splitting the two costs:

- **Refusals on the served tag** — [`models/qualify.py`](../models/qualify.py)
  re-runs heretic's exact `KeywordRate` scorer against the running ollama
  endpoint, comparable to heretic's pre-quant number.  It *imports* heretic's
  matcher from the installed package rather than copying it — the AGPL boundary
  again: heretic is a build/eval-time tool, its code never enters this tree.
- **Quantization KL** — faithful KL needs full next-token distributions, which
  ollama does not expose; so the cost of quantization is measured with
  `llama-perplexity --kl-divergence` between the f16 and quantized GGUFs (the
  canonical tool), not reimplemented against the endpoint.  Heretic's KL is the
  cost of liberation; this is the cost of conversion.

## Provenance, the AGPL boundary, and the trust boundary

- **AGPL stays outside the tree.**  Heretic is a standalone *build tool*,
  invoked out-of-band like `llama.cpp` and `ollama` already are — never
  vendored, never imported into `cloister-worker`, never in `go.mod`.  So it never
  reaches `go-licenses check ./...`, and its copyleft never touches cloister's
  own license posture.  The models it outputs are weights we run locally and do
  not distribute.  This invariant is load-bearing: heretic in the Go tree would
  break the copyleft ban.
- **Provenance becomes an artifact.**  Each liberated tag is a *build output* of
  a tracked recipe — base model + optional commit pin, heretic config, quant
  level, Modelfile.  `liberate.sh` captures heretic's `reproduce.json` and a
  `build-info` stamp alongside the run.  Reproducible, reviewable, unlike a
  download.
- **The build runs host-side, off the jail.**  Abliteration needs egress: it
  pulls the base and heretic's calibration datasets (`mlabonne/harmful_behaviors`,
  `mlabonne/harmless_alpaca`) from Hugging Face.  This is the *same trust
  boundary as `ollama pull` staging today* — the build box is the staging box,
  never the jailed `infer` (which holds no egress and cannot pull).  The
  discipline mirrors the dependency airlock
  ([`bin/update-gradle-deps.bat`](../bin/update-gradle-deps.bat)) and the
  deep-think pull airlock: egress is a deliberate, scripted human act on a box
  that is not the serving jail.

## Packaging

A new top-level [`models/`](../models) tree — the build-side analog of `docker/`
and the planned `mac/`:

```
models/
  README.md                  # what a recipe is; prerequisites; how to build + qualify
  liberate.sh                # the pipeline: heretic -> gguf -> quantize -> ollama create
  qualify.py                 # refusals of the SERVED tag (heretic's scorer, imported)
  qwen3-4b-smoke/            # fast toolchain check (~20-30 min on a 24 GB card)
    recipe.conf              # base id, quant, ollama tag
    heretic.toml             # ablation profile (datasets, trials, quantization)
    Modelfile                # ollama params/template; FROM is filled in by liberate.sh
  qwen3-coder-30b/           # the pilot / A-B target vs. the huihui download
    recipe.conf
    heretic.toml
    Modelfile
```

A "recipe" is a directory: `recipe.conf` (what to build), `heretic.toml` (how to
ablate), `Modelfile` (how to register).  `liberate.sh <recipe-dir>` is the whole
interface.

## Non-functional requirements

- **No heretic in the runtime tree** — build tool only; `go-licenses` stays
  green.
- **Every liberated build is measured, at both stages** — heretic's refusal rate
  and KL on the pre-quant model, *and* the served tag's refusal rate + the
  quantization KL on the artifact `infer` actually loads.  A bad number at either
  stage is a rejected build, not a shipped one.
- **Reproducibility captured** — `reproduce.json` + `build-info` per run.
- **Off-jail egress** — builds never run inside `infer`; `MODELS_DIR` is written
  host-side and mounted read-only, exactly as staging is today.
- **Datasets and base pinned** — a recipe names an exact base (and may pin a
  commit); calibration datasets are heretic's defaults unless a recipe overrides
  them.

## Relationship to the agency

This is the artifact-provenance layer *beneath* the [agency](agency.md)'s runtime
provenance.  The agency answers "which engine served this response"; the recipe
tree answers "how was that engine's weights built."  When the agency lands, its
`(node, model)` fallback chains name liberated tags, and a build's measured
metrics are the natural thing to surface on its status volume.

## Phasing

1. **Pilot (the gate).**  Smoke-test the toolchain on `qwen3-4b-smoke`, then
   build `qwen3-coder-30b` from stock `Qwen/Qwen3-Coder-30B-A3B-Instruct` and
   A/B it against the downloaded `huihui_ai/qwen3-coder-abliterated:30b` on
   heretic's refusal rate + KL, the *served* tag's `qualify.py` refusal rate +
   quantization KL, *and* real cloister coding/security tasks.  Land this doc +
   `models/` only after the pilot; do not merge on faith.
2. **Liberate the working set.**  If the pilot holds: build recipes for the tags
   we actually serve (the coder lanes; the deep-think and think-fast lanes from
   [deepthink.md](deepthink.md)); retire the downloaded `*-abliterated` tags;
   GETTING_STARTED gains a "build a liberated model" section beside "stage model
   weights."
3. **Policy.**  Cloister serves only liberated tags.  Document the stance;
   optionally add a staging check that flags a non-liberated tag in `MODELS_DIR`.
4. **Fold into the agency.**  When the agency lands, its chains reference
   liberated tags and their build metrics surface on the status volume; this doc
   goes from PLANNED to real in [ARCHITECTURE.md](ARCHITECTURE.md).
