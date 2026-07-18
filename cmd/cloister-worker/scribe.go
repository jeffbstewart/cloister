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

package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/jeffbstewart/cloister/internal/scribe"
	"github.com/jeffbstewart/cloister/internal/status/sink"
	"github.com/jeffbstewart/cloister/internal/workspace"
)

// scribeRole parses the scribe's flag set and returns its bootstrap.
func scribeRole(args []string) (func(), error) {
	fs := flag.NewFlagSet("scribe", flag.ContinueOnError)
	common := registerCommon(fs, ":9300")
	wsDir := fs.String("workspace", "/workspace", "project bind mount; the tree the scribe edits")
	stateURL := fs.String("state-url", envOr("STATE_URL", ""), "base URL of the state service")
	stageDir := fs.String("scribe-stage-dir", "/scribe-state",
		"durable staging dir for pending (approval-gated) changes")
	approvals := fs.Bool("scribe-approvals", false,
		"hold gated writes PENDING human approval instead of refusing them")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return common.runOrProbe(func() {
		runScribe(scribeOptions{
			Addr: *common.addr, Workspace: *wsDir, StateURL: *stateURL,
			StageDir: *stageDir, Approvals: *approvals,
		})
	}), nil
}

// scribeOptions carries the scribe's bootstrap inputs.
type scribeOptions struct {
	Addr      string
	Workspace string
	StateURL  string
	StageDir  string
	Approvals bool
}

func runScribe(o scribeOptions) {
	token := os.Getenv("STATE_TOKEN")
	if o.StateURL == "" || token == "" {
		log.Fatalf("scribe needs STATE_URL and STATE_TOKEN: it audits every mutation to the state service")
	}
	root, err := workspace.Open(o.Workspace)
	if err != nil {
		log.Fatalf("scribe: %v", err)
	}
	// One client satisfies Auditor, DiffStore, and ApprovalClient.
	client := sink.NewClient(o.StateURL, token)
	cfg := scribe.Config{
		Version:  version,
		Root:     root,
		Audit:    client,
		Diffs:    client,
		StageDir: o.StageDir,
	}
	if o.Approvals {
		cfg.Approvals = client // hold gated writes pending approval (resolved on the /approvals page)
	}
	srv := scribe.New(cfg)
	srv.Recover() // resume any approvals staged before a restart (no-op when approvals are off)
	serveHTTP(&http.Server{Addr: o.Addr, Handler: srv.Handler()},
		fmt.Sprintf("scribe (workspace %s → state %s)", o.Workspace, o.StateURL))
}
