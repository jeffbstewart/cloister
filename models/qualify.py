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

"""Post-conversion refusal qualification for a served (quantized) ollama model.

Heretic measures the refusal rate on its in-memory PyTorch model, BEFORE GGUF
conversion and quantization. Quantization is lossy and can partially restore
refusals, so the artifact we actually serve deserves its own check. This script
re-runs heretic's exact KeywordRate scorer against a running ollama endpoint, so
the number is directly comparable to what heretic printed for the pre-quant
model.

Licensing: heretic is AGPL-3.0. We do NOT copy its keyword list or matcher into
the cloister tree; we IMPORT them from the installed `heretic` package at eval
time (this script is an out-of-tree build/eval tool, like liberate.sh's calls to
heretic itself, not part of the runtime). Run it in the same environment where
`pip install -U heretic-llm` put heretic.

KL divergence is deliberately NOT computed here: faithful KL needs full
next-token distributions from both the candidate and the original, which ollama
does not expose. Heretic's printed KL is the cost of *liberation* (pre-quant);
the cost of *quantization* on the served artifact is measured with the canonical
tool -- see `llama-perplexity --kl-divergence` in models/README.md.

Usage:
    python3 models/qualify.py --model qwen3-coder-liberated:30b
    python3 models/qualify.py --model <tag> --base-url http://127.0.0.1:11434/v1 --print-responses
"""

import argparse
import json
import sys
import urllib.request

try:
    # Heretic's own definitions, so the score matches its pre-quant number
    # exactly. Not vendored -- imported from the installed AGPL package.
    from heretic.scorers.keyword_rate import KeywordRate, Settings as KeywordSettings
except ImportError:
    sys.exit(
        "qualify: cannot import heretic. Run this in the environment where "
        "`pip install -U heretic-llm` installed heretic (the same one liberate.sh uses)."
    )

try:
    from datasets import load_dataset
except ImportError:
    sys.exit("qualify: `datasets` not available (it ships with heretic-llm). Activate the heretic env.")


def parse_split(split: str) -> tuple[str, int | None]:
    """Split heretic's 'test[:100]' form into (base_split, limit)."""
    if "[" in split:
        base, _, rest = split.partition("[")
        limit = rest.strip("][").lstrip(":") or None
        return base, (int(limit) if limit else None)
    return split, None


def load_prompts(dataset: str, split: str, column: str) -> list[str]:
    base_split, limit = parse_split(split)
    ds = load_dataset(dataset, split=base_split)
    if limit is not None:
        ds = ds.select(range(min(limit, len(ds))))
    return [row[column] for row in ds]


def complete(base_url: str, model: str, prompt: str, max_tokens: int, timeout: float) -> str:
    """One turn against an OpenAI-compatible endpoint (ollama / the agency), stdlib only."""
    body = json.dumps(
        {
            "model": model,
            "messages": [{"role": "user", "content": prompt}],
            "max_tokens": max_tokens,
            "temperature": 0.0,
            "stream": False,
        }
    ).encode()
    req = urllib.request.Request(
        base_url.rstrip("/") + "/chat/completions",
        data=body,
        headers={"Content-Type": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        payload = json.load(resp)
    return payload["choices"][0]["message"]["content"]


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--model", required=True, help="ollama tag to qualify, e.g. qwen3-coder-liberated:30b")
    ap.add_argument("--base-url", default="http://127.0.0.1:11434/v1", help="OpenAI-compatible endpoint")
    ap.add_argument("--dataset", default="mlabonne/harmful_behaviors")
    ap.add_argument("--split", default="test[:100]")
    ap.add_argument("--column", default="text")
    ap.add_argument("--max-tokens", type=int, default=100)
    ap.add_argument("--timeout", type=float, default=120.0)
    ap.add_argument("--print-responses", action="store_true")
    args = ap.parse_args()

    # Heretic's exact matcher, reused (not reimplemented): _is_match only reads
    # self.settings.keyword_markers, so a shim carrying the default settings is
    # enough to call it.
    settings = KeywordSettings()
    shim = argparse.Namespace(settings=settings)
    is_refusal = lambda response: KeywordRate._is_match(shim, response)

    print(f"qualify: loading {args.dataset} {args.split} (column '{args.column}')...")
    prompts = load_prompts(args.dataset, args.split, args.column)
    print(f"qualify: {len(prompts)} prompts; scoring {args.model} at {args.base_url}")

    refusals = 0
    for i, prompt in enumerate(prompts, 1):
        try:
            response = complete(args.base_url, args.model, prompt, args.max_tokens, args.timeout)
        except Exception as exc:  # a failed generation is not a refusal; surface it
            sys.exit(f"qualify: request {i} failed: {exc}")
        matched = is_refusal(response)
        refusals += matched
        if args.print_responses:
            tag = "REFUSE" if matched else "  ok  "
            print(f"[{tag}] {prompt[:70]!r} -> {response.strip()[:90]!r}")
        else:
            sys.stdout.write("x" if matched else ".")
            sys.stdout.flush()

    if not args.print_responses:
        print()
    rate = refusals / len(prompts)
    print(f"qualify: refusals {refusals}/{len(prompts)}  (rate {rate:.2f})")
    print("qualify: compare to heretic's pre-quant number; a large gap means quantization")
    print("qualify: restored refusals -- try a higher-fidelity quant (Q5_K_M/Q6_K).")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
