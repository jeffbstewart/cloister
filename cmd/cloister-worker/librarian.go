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
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jeffbstewart/cloister/internal/agency"
	"github.com/jeffbstewart/cloister/internal/infer"
	"github.com/jeffbstewart/cloister/internal/librarian"
	"github.com/jeffbstewart/cloister/internal/openai"
	"github.com/jeffbstewart/cloister/internal/repo"
	"github.com/jeffbstewart/cloister/internal/status/sink"
	"github.com/jeffbstewart/cloister/internal/watch"
)

// librarianRole parses the librarian's flag set and returns its bootstrap.
func librarianRole(args []string) (func(), error) {
	fs := flag.NewFlagSet("librarian", flag.ContinueOnError)
	common := registerCommon(fs, ":9400")
	workspace := fs.String("workspace", "/workspace", "project bind mount; the tree the librarian serves")
	stateURL := fs.String("state-url", envOr("STATE_URL", ""), "base URL of the state service")
	budgetMB := fs.Int("repo-budget-mb", 256, "total resident-content cap for the in-memory model")
	maxFileMB := fs.Int("repo-max-file-mb", 2, "per-file cap; larger files are metadata-only")
	rescanInterval := fs.Duration("rescan-interval", 30*time.Minute,
		"how often to re-walk the workspace for host edits the watcher misses")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return common.runOrProbe(func() {
		runLibrarian(librarianOptions{
			Addr: *common.addr, Workspace: *workspace, StateURL: *stateURL,
			BudgetMB: *budgetMB, MaxFileMB: *maxFileMB,
			RescanInterval: *rescanInterval,
		})
	}), nil
}

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
// with OPENAI_BASE_URL set it returns an *infer.Client; unset (or a config
// error) it logs that comprehension is disabled and returns nil, so the
// librarian still boots with its mechanical tools.
func buildInferencer() librarian.Inferencer {
	baseURL := envOr("OPENAI_BASE_URL", "")
	if baseURL == "" {
		log.Printf("librarian: OPENAI_BASE_URL unset — comprehension tools disabled (mechanical-only)")
		return nil
	}
	// Each effort asks the agency for its ENGINE CLASS — the model field
	// carries a class name, never a model tag; the agency's routing policy
	// picks the node and model (docs/agency.md).  The provenance Names are
	// the class names, so the footer reads exactly what was routed.
	engineFor := func(class string) infer.Engine {
		return infer.Engine{Name: class, Completer: openai.New(openai.Options{
			BaseURL: baseURL,
			Model:   class,
			Key:     os.Getenv("OPENAI_API_KEY"),
			Caller:  "librarian",
		})}
	}
	client, err := infer.New(infer.Config{
		Engines: map[infer.Effort]infer.Engine{
			infer.Quick:    engineFor(agency.ClassThinkFast),
			infer.Thorough: engineFor(agency.ClassDeepThink),
		},
	})
	if err != nil {
		log.Printf("librarian: inference client build failed (%v) — comprehension tools disabled (mechanical-only)", err)
		return nil
	}
	log.Printf("librarian: comprehension tools enabled → classes %s/%s @ %s",
		agency.ClassThinkFast, agency.ClassDeepThink, baseURL)
	return client
}
