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

// forge-lint verifies that the forge repository enforces the grange
// design's safety requirements R1-R8 (docs/grange.md, "Forge
// requirements"): the PR gate the whole containment story leans on.  It is
// compose-lint's sibling — compose-lint guards the topology we deploy,
// forge-lint guards the merge path we trust.
//
// Run it with an OPERATOR credential, outside any cell:
//
//	FORGE_LINT_TOKEN=<admin PAT> go run ./cmd/forge-lint
//
// The token comes from FORGE_LINT_TOKEN (falling back to GITHUB_TOKEN,
// then GH_TOKEN); the bot's token must never be used — it must not be able
// to read what this lint reads.  Sections the credential cannot read
// report UNVERIFIED and fail the run; -allow-unverified downgrades them to
// warnings for limited-credential contexts (CI's read-only token).
//
// Exit: 0 clean, 1 violations (or unverified without -allow-unverified),
// 2 config or forge-API errors.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jeffbstewart/cloister/internal/forgelint"
)

func main() {
	fs := flag.NewFlagSet("forge-lint", flag.ContinueOnError)
	configPath := fs.String("config", "etc/forge-lint.yaml", "path of the forge-lint config yaml")
	allowUnverified := fs.Bool("allow-unverified", false, "exit 0 even when sections were unreadable with this credential (CI's limited token); violations still fail")
	timeout := fs.Duration("timeout", time.Minute, "total budget for the forge API reads")
	if err := fs.Parse(os.Args[1:]); err != nil {
		if err == flag.ErrHelp {
			os.Exit(0)
		}
		os.Exit(2)
	}

	cfg, err := forgelint.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	token := firstEnv("FORGE_LINT_TOKEN", "GITHUB_TOKEN", "GH_TOKEN")
	var forge interface {
		Snapshot(context.Context) (*forgelint.Snapshot, error)
	}
	switch cfg.Forge {
	case forgelint.ForgeGitHub:
		forge = forgelint.NewGitHub(cfg, token, nil)
	case forgelint.ForgeGitea:
		forge = forgelint.NewGitea(cfg, token, nil)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	snap, err := forge.Snapshot(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forge-lint: %s %s: %v\n", cfg.Forge, cfg.Repo, err)
		os.Exit(2)
	}

	exit := 0
	for _, v := range forgelint.Check(cfg, snap) {
		fmt.Printf("forge-lint: %s %s — %s\n", v.Req, v.Status, v.Detail)
		switch v.Status {
		case forgelint.Violation:
			exit = 1
		case forgelint.Unverified:
			if !*allowUnverified {
				exit = 1
			}
		}
	}
	if exit == 0 {
		fmt.Printf("forge-lint: %s %s OK — the merge path enforces R1-R8 as far as this credential can see\n", cfg.Forge, cfg.Repo)
	} else {
		fmt.Println("forge-lint: hardening runbook: " + forgelint.HardeningRunbook)
	}
	os.Exit(exit)
}

// firstEnv returns the first non-empty value among the named variables.
func firstEnv(names ...string) string {
	for _, name := range names {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return ""
}
