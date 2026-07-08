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
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jeffbstewart/cloister/internal/infer"
	"github.com/jeffbstewart/cloister/internal/librarian"
	"github.com/jeffbstewart/cloister/internal/openai"
	"github.com/jeffbstewart/cloister/internal/repo"
	"github.com/jeffbstewart/cloister/internal/status/sink"
	"github.com/jeffbstewart/cloister/internal/watch"
)

// librarianOptions carries the librarian's bootstrap inputs.
type librarianOptions struct {
	Addr           string
	Workspace      string
	StateURL       string
	BudgetMB       int
	MaxFileMB      int
	RescanInterval time.Duration
}

func runLibrarian(o librarianOptions) {
	token := os.Getenv("STATE_TOKEN")
	if o.StateURL == "" || token == "" {
		log.Fatalf("librarian needs STATE_URL and STATE_TOKEN: it audits read denials to the state service")
	}
	// The initial scan reads every visible file into the model — it can
	// take a while on a large tree, so bracket it with progress logs
	// (stderr, so `docker logs` shows them).
	log.Printf("librarian: scanning workspace %s into memory (this can take a while) ...", o.Workspace)
	scanStart := time.Now()
	rep, err := repo.New(o.Workspace, repo.Config{
		Budget:      int64(o.BudgetMB) << 20,
		MaxFileSize: int64(o.MaxFileMB) << 20,
	})
	if err != nil {
		log.Fatalf("librarian: %v", err) // fail loud: the over-budget message names the offenders
	}
	st := rep.ScanStats()
	log.Printf("librarian: workspace scan complete in %s (metadata walk %s + content read %s)",
		time.Since(scanStart).Round(time.Millisecond), st.Walk.Round(time.Millisecond), st.Read.Round(time.Millisecond))

	// Watcher-primary freshness (the spike verdict): container writers
	// arrive as events; the minute rescan bounds host-edit staleness and
	// is the whole story on platforms without a watcher.
	w, err := watch.New(o.Workspace, rep.Watchable, rep.Invalidate, func() {
		if err := rep.Rescan(); err != nil {
			log.Printf("librarian: overflow rescan: %v", err)
		}
	})
	switch {
	case errors.Is(err, watch.ErrUnsupported):
		log.Printf("librarian: no filesystem watcher on this platform; rescan-only freshness")
	case err != nil:
		log.Fatalf("librarian: start watcher: %v", err)
	default:
		defer w.Close()
	}
	go func() {
		tick := time.NewTicker(o.RescanInterval)
		defer tick.Stop()
		for range tick.C {
			if err := rep.Rescan(); err != nil {
				log.Printf("librarian: rescan: %v", err)
				continue
			}
			// The rescan repeats every interval; if one eats more than a
			// tenth of it, the metadata walk is too costly for this cadence
			// (back off the interval, or the watcher is carrying freshness
			// anyway — the rescan only catches host edits).
			if st := rep.ScanStats(); st.Total() > o.RescanInterval/10 {
				log.Printf("librarian: SLOW rescan %s (metadata walk %s + content read %s) — over 10%% of the %s interval",
					st.Total().Round(time.Millisecond), st.Walk.Round(time.Millisecond),
					st.Read.Round(time.Millisecond), o.RescanInterval)
			}
		}
	}()

	srv := librarian.New(librarian.Config{
		Version: version,
		Repo:    rep,
		Audit:   sink.NewClient(o.StateURL, token),
		Infer:   buildInferencer(),
	})

	// Boot diagnostic: what's resident and what's heaviest, so an
	// unexpectedly large model is explained by name (tune the ignore
	// files) rather than guessed at.
	report := rep.Report(15)
	log.Printf("librarian: in-memory model — %d files, %d MiB resident of %d MiB budget",
		report.Files, report.Bytes>>20, report.Budget>>20)
	for _, e := range report.Largest {
		log.Printf("librarian:   %7d KiB  %s", e.Size>>10, e.Path)
	}

	serveHTTP(&http.Server{Addr: o.Addr, Handler: srv.Handler()},
		fmt.Sprintf("librarian (workspace %s, %d/%d MiB resident → state %s)",
			o.Workspace, report.Bytes>>20, report.Budget>>20, o.StateURL))
}

// buildInferencer wires the comprehension inference client from env, fail-soft:
// with OPENAI_BASE_URL and OPENAI_MODEL both set it returns an *infer.Client;
// with either unset (or a config error) it logs that comprehension is disabled
// and returns nil, so the librarian still boots with its mechanical tools.
func buildInferencer() librarian.Inferencer {
	baseURL := envOr("OPENAI_BASE_URL", "")
	model := os.Getenv("OPENAI_MODEL")
	if baseURL == "" || model == "" {
		log.Printf("librarian: OPENAI_BASE_URL/OPENAI_MODEL unset — comprehension tools disabled (mechanical-only)")
		return nil
	}
	// Both efforts resolve to the same endpoint today; the agency will later
	// provide distinct engine classes (deep-think on a separate node).  The
	// provenance Names differ now so the footer already reads the way it will.
	engine := openai.New(openai.Options{
		BaseURL: baseURL,
		Model:   model,
		Key:     os.Getenv("OPENAI_API_KEY"),
	})
	client, err := infer.New(infer.Config{
		Engines: map[infer.Effort]infer.Engine{
			infer.Quick:    {Name: "think-fast", Completer: engine},
			infer.Thorough: {Name: "deep-think", Completer: engine},
		},
	})
	if err != nil {
		log.Printf("librarian: inference client build failed (%v) — comprehension tools disabled (mechanical-only)", err)
		return nil
	}
	log.Printf("librarian: comprehension tools enabled → model %s @ %s", model, baseURL)
	return client
}
