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

	"github.com/jeffbstewart/cloister/internal/librarian"
	"github.com/jeffbstewart/cloister/internal/repo"
	"github.com/jeffbstewart/cloister/internal/status/sink"
	"github.com/jeffbstewart/cloister/internal/watch"
)

// librarianOptions carries the librarian's bootstrap inputs.
type librarianOptions struct {
	Addr      string
	Workspace string
	StateURL  string
	BudgetMB  int
	MaxFileMB int
}

func runLibrarian(o librarianOptions) {
	token := os.Getenv("STATE_TOKEN")
	if o.StateURL == "" || token == "" {
		log.Fatalf("librarian needs STATE_URL and STATE_TOKEN: it audits read denials to the state service")
	}
	rep, err := repo.New(o.Workspace, repo.Config{
		Budget:      int64(o.BudgetMB) << 20,
		MaxFileSize: int64(o.MaxFileMB) << 20,
	})
	if err != nil {
		log.Fatalf("librarian: %v", err) // fail loud: the over-budget message names the offenders
	}

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
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for range tick.C {
			if err := rep.Rescan(); err != nil {
				log.Printf("librarian: rescan: %v", err)
			}
		}
	}()

	srv := librarian.New(librarian.Config{
		Version: version,
		Repo:    rep,
		Audit:   sink.NewClient(o.StateURL, token),
	})
	spent, budget := rep.Resident()
	serveHTTP(&http.Server{Addr: o.Addr, Handler: srv.Handler()},
		fmt.Sprintf("librarian (workspace %s, %d/%d MiB resident → state %s)",
			o.Workspace, spent>>20, budget>>20, o.StateURL))
}
