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
