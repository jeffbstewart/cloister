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
	"path/filepath"
	"time"

	"github.com/jeffbstewart/cloister/internal/egress"
	"github.com/jeffbstewart/cloister/internal/egress/policy"
	"github.com/jeffbstewart/cloister/internal/egress/wire"
	"github.com/jeffbstewart/cloister/internal/openai"
	"github.com/jeffbstewart/cloister/internal/scholar"
	"github.com/jeffbstewart/cloister/internal/status/sink"
)

// scholarRole parses the scholar's flag set and returns its bootstrap.
func scholarRole(args []string) (func(), error) {
	fs := flag.NewFlagSet("scholar", flag.ContinueOnError)
	common := registerCommon(fs, ":9500")
	policyPath := fs.String("policy", "/etc/scholar/policy.yaml", "read-only egress policy file")
	burnDir := fs.String("burn-dir", "/burn",
		"writable volume for the burn-rate ledger (timestamps only)")
	stateURL := fs.String("state-url", envOr("STATE_URL", ""), "base URL of the state service")
	answerGate := fs.Bool("answer-gate", true,
		"gate the answer on operator approval before returning it")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return common.runOrProbe(func() {
		runScholar(scholarOptions{
			Addr: *common.addr, PolicyPath: *policyPath, BurnDir: *burnDir,
			StateURL: *stateURL, AnswerGate: *answerGate,
		})
	}), nil
}

// scholarOptions carries the scholar's bootstrap inputs.
type scholarOptions struct {
	Addr       string
	PolicyPath string
	BurnDir    string
	StateURL   string
	AnswerGate bool
}

func runScholar(o scholarOptions) {
	// The fail-closed egress self-check runs FIRST: if the scholar can
	// reach the arbitrary internet, it must not start.
	if err := scholar.AssertNoPublicEgress(); err != nil {
		log.Fatalf("scholar: %v", err)
	}
	log.Printf("scholar: egress self-check passed — no arbitrary internet route (relay=%s)", os.Getenv("KAGI_RELAY_ADDR"))

	token := os.Getenv("STATE_TOKEN")
	if o.StateURL == "" || token == "" {
		log.Fatalf("scholar needs STATE_URL and STATE_TOKEN: it audits every search/extract to the state service")
	}
	baseURL := envOr("OPENAI_BASE_URL", "")
	model := os.Getenv("OPENAI_MODEL")
	if baseURL == "" || model == "" {
		log.Fatalf("scholar needs OPENAI_BASE_URL and OPENAI_MODEL")
	}

	p, err := policy.LoadPolicy(o.PolicyPath)
	if err != nil {
		log.Fatalf("scholar: %v", err)
	}
	kagiKey, braveKey := os.Getenv("KAGI_API_KEY"), os.Getenv("BRAVE_API_KEY")
	searcher, retriever, err := egress.NewProviders(egress.ProviderOptions{
		Policy:     p,
		KagiKey:    kagiKey,
		KagiRelay:  os.Getenv("KAGI_RELAY_ADDR"),
		BraveKey:   braveKey,
		BraveRelay: os.Getenv("BRAVE_RELAY_ADDR"),
	})
	if err != nil {
		log.Fatalf("scholar: %v", err)
	}
	now := time.Now()
	searchLedger, err := egress.OpenLedger(filepath.Join(o.BurnDir, "search"), 48*time.Hour, now)
	if err != nil {
		log.Fatalf("scholar: %v", err)
	}
	extractLedger, err := egress.OpenLedger(filepath.Join(o.BurnDir, "extract"), 48*time.Hour, now)
	if err != nil {
		log.Fatalf("scholar: %v", err)
	}
	sub, err := egress.NewSubsystem(egress.Config{
		Policy:        p,
		Searcher:      searcher,
		Retriever:     retriever,
		SearchLedger:  searchLedger,
		ExtractLedger: extractLedger,
		Scrubber:      wire.NewScrubber(kagiKey, braveKey),
	})
	if err != nil {
		log.Fatalf("scholar: %v", err)
	}

	stateClient := sink.NewClient(o.StateURL, token) // one client: audit + approvals + transcripts
	srv := scholar.New(scholar.Config{
		Version: version,
		Egress:  sub,
		Model: openai.New(openai.Options{
			BaseURL: baseURL,
			Model:   model,
			Key:     os.Getenv("OPENAI_API_KEY"),
			Caller:  "scholar",
		}),
		Audit:       stateClient,
		Approvals:   stateClient,
		Transcripts: stateClient,
		AnswerGate:  o.AnswerGate,
		Caps:        scholar.DefaultCaps(),
	})
	serveHTTP(&http.Server{Addr: o.Addr, Handler: srv.Handler()},
		fmt.Sprintf("scholar (research → model %s @ %s, engine %s)", model, baseURL, sub.Engine()))
}
