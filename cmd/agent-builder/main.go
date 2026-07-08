// Copyright 2026 Jeffrey B. Stewart
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// agent-builder is the one binary of the Cloister cell — every worker is a
// mode of it, selected by the REQUIRED -worker-mode flag:
//
//	builder        reads /workspace/agent-harness.yaml, serves the declared
//	               actions as MCP tools on :9200, and streams
//	               logs/audit/status to the state service.  Holds NO /state
//	               mount — agent-authored build code cannot touch the record
//	               of what it did.
//	state-service  owns the /state volume, accepts the workers'
//	               authenticated appends, and serves the read-only status
//	               pages on :9201.  No egress; reachable from the host only
//	               via the socat status relay.
//	scribe         the workspace editor: the sole audited writer of
//	               /workspace, serving confined write ops as MCP tools on
//	               :9300 and auditing each to the state service.
//	scholar        the quarantined web-research agent: one research MCP
//	               tool, reaching the web only through its pinned relay,
//	               refusing to start if any other route exists.
//	librarian      the read side of the cell: mechanical read tools on
//	               :9400 served from an in-memory, shield-filtered model
//	               of the workspace; denials (only) audited to state.
//
// This file is only the front door: flags and the mode dispatch.  Each
// worker's bootstrap lives in its own file (builder.go, scribe.go,
// scholar.go, statesvc.go, librarian.go); shared serving plumbing in
// serve.go.
package main

import (
	"flag"
	"log"
	"os"
	"time"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

// workerMode selects which worker this process runs as.  There is
// deliberately no default: a cell's compose file must say what each
// container is, and the incompatible per-mode flags cannot be combined.
type workerMode string

const (
	modeBuilder      workerMode = "builder"
	modeStateService workerMode = "state-service"
	modeScribe       workerMode = "scribe"
	modeScholar      workerMode = "scholar"
	modeLibrarian    workerMode = "librarian"
)

const workerModes = "builder | state-service | scribe | scholar | librarian"

func main() {
	mode := flag.String("worker-mode", "", "REQUIRED: which worker this process is — "+workerModes)
	addr := flag.String("addr", ":9200", "listen address")
	workspace := flag.String("workspace", "/workspace", "project bind mount; actions run here")
	spoolDir := flag.String("spool", "/spool", "local (tmpfs) log spool for digests and get_log")
	stateDir := flag.String("state-dir", "/state", "state-service volume: logs, audit, status")
	stateURL := flag.String("state-url", envOr("STATE_URL", ""), "builder: base URL of the state service")
	toolchainFile := flag.String("toolchain-file", "/etc/agent-builder/toolchain",
		"file holding this image's toolchain id")
	scribeStageDir := flag.String("scribe-stage-dir", "/scribe-state",
		"scribe: durable staging dir for pending (approval-gated) changes")
	scribeApprovals := flag.Bool("scribe-approvals", false,
		"scribe: hold gated writes PENDING human approval instead of refusing them")
	scholarPolicy := flag.String("policy", "/etc/scholar/policy.yaml",
		"scholar: read-only egress policy file")
	burnDir := flag.String("burn-dir", "/burn",
		"scholar: writable volume for the burn-rate ledger (timestamps only)")
	answerGate := flag.Bool("answer-gate", true,
		"scholar: gate the answer on operator approval before returning it")
	repoBudgetMB := flag.Int("repo-budget-mb", 256,
		"librarian: total resident-content cap for the in-memory model")
	repoMaxFileMB := flag.Int("repo-max-file-mb", 2,
		"librarian: per-file cap; larger files are metadata-only")
	rescanInterval := flag.Duration("rescan-interval", 30*time.Minute,
		"librarian: how often to re-walk the workspace for host edits the watcher misses")
	healthcheck := flag.Bool("healthcheck", false,
		"probe the local /healthz and exit 0/1 (container HEALTHCHECK)")
	flag.Parse()

	if *healthcheck {
		os.Exit(probeHealthz(*addr))
	}

	// Local time in log timestamps: honors the container's TZ (embedded
	// tzdata makes any IANA zone work).  Data timestamps — audit records,
	// ledgers — stay UTC by their own code; this is only the log prefix.
	log.SetFlags(log.LstdFlags)

	switch workerMode(*mode) {
	case modeBuilder:
		runBuilder(builderOptions{
			Addr: *addr, Workspace: *workspace, SpoolDir: *spoolDir,
			StateURL: *stateURL, ToolchainFile: *toolchainFile,
		})
	case modeStateService:
		runStateService(stateOptions{Addr: *addr, StateDir: *stateDir})
	case modeScribe:
		runScribe(scribeOptions{
			Addr: *addr, Workspace: *workspace, StateURL: *stateURL,
			StageDir: *scribeStageDir, Approvals: *scribeApprovals,
		})
	case modeScholar:
		runScholar(scholarOptions{
			Addr: *addr, PolicyPath: *scholarPolicy, BurnDir: *burnDir,
			StateURL: *stateURL, AnswerGate: *answerGate,
		})
	case modeLibrarian:
		runLibrarian(librarianOptions{
			Addr: *addr, Workspace: *workspace, StateURL: *stateURL,
			BudgetMB: *repoBudgetMB, MaxFileMB: *repoMaxFileMB,
			RescanInterval: *rescanInterval,
		})
	case "":
		log.Fatalf("-worker-mode is required: %s", workerModes)
	default:
		log.Fatalf("unknown -worker-mode %q: want %s", *mode, workerModes)
	}
}
