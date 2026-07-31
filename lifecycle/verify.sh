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

# verify.sh — the full local pre-PR gate set (CLAUDE.md "Build & verify"),
# in one command so it is never typed by hand or run half-way.  CI runs the
# same checks; this is the fast local mirror.
#
# Usage (from anywhere in the repo):
#   bash lifecycle/verify.sh
#
# Exit code: 0 if every gate passes, non-zero at the first that fails.

set -euo pipefail

# Run from the repo root regardless of the caller's cwd.
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

step=0
run() {
	step=$((step + 1))
	echo ""
	echo "== [$step] $* =="
	"$@"
}

# gofmt reports its findings on stdout instead of a non-zero exit, so a
# non-empty listing is the failure signal — handle it specially.
check_gofmt() {
	step=$((step + 1))
	echo ""
	echo "== [$step] gofmt -l . (must be empty) =="
	local unformatted
	unformatted="$(gofmt -l .)"
	if [ -n "$unformatted" ]; then
		echo "gofmt: these files are not formatted:" >&2
		echo "$unformatted" >&2
		return 1
	fi
}

run go build ./...
run env GOOS=linux go build ./... # the deploy target; catches build-tag splits a host build misses
run go test ./...
check_gofmt
run go vet ./...
run go-licenses check ./... # deny copyleft
run go run ./cmd/compose-lint docker/ai-workers.yaml docker/inference.yaml
run go run ./cmd/copyright-lint # headers present + year current

echo ""
echo "== verify: all $step gates passed =="
