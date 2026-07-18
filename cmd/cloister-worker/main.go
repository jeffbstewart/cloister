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

// cloister-worker is the one binary of Cloister — every worker, in the
// cell or the shared infra stack, is a role of it.  It is a multi-call
// binary: the workers image bakes one hard link per role name, and the
// program name (argv[0] basename) selects the role —
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
//	               tool on :9500, reaching the web only through its pinned
//	               relay, refusing to start if any other route exists.
//	librarian      the read side of the cell: mechanical read tools on
//	               :9400 served from an in-memory, shield-filtered model
//	               of the workspace; denials (only) audited to state.
//	agency         the sole inference door on :11434, in the shared infra
//	               stack (not the cell): a streaming pass-through of the
//	               OpenAI-compatible /v1 API to the model server, which
//	               consumers can no longer reach directly.
//
// Each role parses its own flag set, so a flag from the wrong role is a
// startup error, never a silent no-op.  Under the generic binary name
// (`cloister-worker` — local runs, filesystems without hard links, and,
// via the image's `agent-builder` compat link, pre-multi-call compose
// files) a LEADING `-worker-mode <role>` selects the role instead, and a
// leading `-healthcheck` keeps the old container HEALTHCHECK form
// working.  There is deliberately no default role: the program name or
// the selector must say what this process is.
//
// This file is only the front door: role resolution and dispatch.  Each
// role's flag set and bootstrap live in its own file (builder.go,
// scribe.go, scholar.go, statesvc.go, librarian.go, agency.go); shared
// serving plumbing in serve.go.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

// roleParser parses one role's command line — everything after the role
// selector — with that role's own flag set and returns the action to run.
// Parsing has no side effects, so a flag error never leaves a half-started
// worker.
type roleParser func(args []string) (run func(), err error)

// roles maps each program name — a hard link baked into the workers image —
// to its parser.  The same names are the values `-worker-mode` accepts
// under the generic binary name.
var roles = map[string]roleParser{
	"builder":       builderRole,
	"state-service": stateServiceRole,
	"scribe":        scribeRole,
	"scholar":       scholarRole,
	"librarian":     librarianRole,
	"agency":        agencyRole,
}

const workerModes = "builder | state-service | scribe | scholar | librarian | agency"

// healthcheckName is the pseudo-role behind the generic `-healthcheck`
// form.  It is resolved only from that leading flag — `-worker-mode
// healthcheck` stays an unknown role, and no image link carries the name.
const healthcheckName = "healthcheck"

func main() {
	// Local time in log timestamps: honors the container's TZ (embedded
	// tzdata makes any IANA zone work).  Data timestamps — audit records,
	// ledgers — stay UTC by their own code; this is only the log prefix.
	log.SetFlags(log.LstdFlags)

	name, roleArgs, err := resolveRole(filepath.Base(os.Args[0]), os.Args[1:])
	if err != nil {
		log.Fatal(err)
	}
	run, err := parserFor(name)(roleArgs)
	if errors.Is(err, flag.ErrHelp) {
		return // -h/-help: the flag set already printed usage
	}
	if err != nil {
		// The role's flag set already printed the problem.  2 is the flag
		// package's own usage-error code (what ExitOnError would exit
		// with); 1 stays the "worker started and failed" code.
		os.Exit(2)
	}
	run()
}

// resolveRole picks the role: from the program name when it is one of the
// role links (`.exe` trimmed for Windows builds), else — under the generic
// or an unrecognized name, where nothing is implied — from a leading
// `-worker-mode <role>` selector or the pre-multi-call `-healthcheck`
// probe form.  It fails closed: no selector, an unknown role, or a
// non-selector first flag is an error naming the roles.
func resolveRole(prog string, args []string) (name string, roleArgs []string, err error) {
	prog = strings.TrimSuffix(prog, ".exe")
	if _, ok := roles[prog]; ok {
		return prog, args, nil
	}
	if len(args) == 0 {
		return "", nil, fmt.Errorf("-worker-mode is required: %s", workerModes)
	}
	sel := args[0]
	switch {
	case sel == "-worker-mode" || sel == "--worker-mode":
		if len(args) < 2 {
			return "", nil, fmt.Errorf("-worker-mode needs a value: %s", workerModes)
		}
		return roleByName(args[1], args[2:])
	case strings.HasPrefix(sel, "-worker-mode="):
		return roleByName(strings.TrimPrefix(sel, "-worker-mode="), args[1:])
	case strings.HasPrefix(sel, "--worker-mode="):
		return roleByName(strings.TrimPrefix(sel, "--worker-mode="), args[1:])
	case sel == "-healthcheck" || sel == "--healthcheck":
		return healthcheckName, args, nil
	default:
		return "", nil, fmt.Errorf(
			"under the generic binary name, -worker-mode must come first (got %q); role links need no selector — %s",
			sel, workerModes)
	}
}

// roleByName validates a -worker-mode value against the role table.
func roleByName(name string, rest []string) (string, []string, error) {
	if _, ok := roles[name]; !ok {
		return "", nil, fmt.Errorf("unknown -worker-mode %q: want %s", name, workerModes)
	}
	return name, rest, nil
}

// parserFor returns the parser resolveRole selected; the pseudo-role rides
// beside the table so it never becomes a -worker-mode value.
func parserFor(name string) roleParser {
	if name == healthcheckName {
		return healthcheckRole
	}
	return roles[name]
}

// healthcheckRole keeps the pre-multi-call container HEALTHCHECK form
// working under the generic name: `agent-builder -healthcheck -addr :9200`.
// Role links probe via their own flag sets (`scribe -healthcheck`), where
// -addr defaults to the role's port.
func healthcheckRole(args []string) (func(), error) {
	fs := flag.NewFlagSet(healthcheckName, flag.ContinueOnError)
	common := registerCommon(fs, ":9200")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	// The leading -healthcheck is what routed here, so probing is the whole
	// job — there is no serve path to fall back to.
	addr := *common.addr
	return func() { os.Exit(probeHealthz(addr)) }, nil
}
