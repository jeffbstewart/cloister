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

	"github.com/jeffbstewart/cloister/internal/archive"
	"github.com/jeffbstewart/cloister/internal/archivist"
)

// archivistRole parses the archivist's flag set and returns its bootstrap.
func archivistRole(args []string) (func(), error) {
	fs := flag.NewFlagSet("archivist", flag.ContinueOnError)
	common := registerCommon(fs, ":9600")
	workspace := fs.String("workspace", "/workspace", "the provisioned checkout the archivist drives")
	defBranch := fs.String("default-branch", "",
		"override default-branch detection (hand-built checkouts only; a grange clone's origin/HEAD is authoritative)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return common.runOrProbe(func() {
		runArchivist(archivistOptions{
			Addr: *common.addr, Workspace: *workspace, DefaultBranch: *defBranch,
		})
	}), nil
}

// archivistOptions carries the archivist's bootstrap inputs.
type archivistOptions struct {
	Addr          string
	Workspace     string
	DefaultBranch string
}

// todoIdentity is the checkpoint identity until the endpoint table lands
// (archivist M1 step 3): identity belongs to the endpoint — one bot per
// (human, forge) — so plumbing it through flags here would build a seam
// step 3 immediately deletes.  Nothing exercises checkpoints in a
// deployed cell yet (the archivist has no compose entry), so the
// placeholder only ever signs local-run and test commits, where its name
// makes any leak into real history unmistakable.
var todoIdentity = archive.Identity{Name: "TODO", Email: "todo@cloister.invalid"}

func runArchivist(o archivistOptions) {
	var opts []archive.Option
	if o.DefaultBranch != "" {
		opts = append(opts, archive.WithDefaultBranch(o.DefaultBranch))
	}
	a, err := archive.New(o.Workspace, todoIdentity, opts...)
	if err != nil {
		log.Fatalf("archivist: %v", err)
	}
	srv := archivist.New(archivist.Config{Version: version, Archive: a})
	// Close before any fatal exit, not deferred past one: log.Fatalf
	// skips defers, and a crash-looping container would leak one hooks
	// dir per restart.
	serveErr := serveHTTP(&http.Server{Addr: o.Addr, Handler: srv.Handler()},
		fmt.Sprintf("archivist (workspace %s, default branch %s)", o.Workspace, a.DefaultBranch()))
	a.Close()
	if serveErr != nil {
		log.Fatalf("serve: %v", serveErr)
	}
}
