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
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/jeffbstewart/cloister/internal/manifest"
	"github.com/jeffbstewart/cloister/internal/mcpserver"
	"github.com/jeffbstewart/cloister/internal/runid"
	"github.com/jeffbstewart/cloister/internal/runner"
	"github.com/jeffbstewart/cloister/internal/status/sink"
)

// builderOptions carries the builder's bootstrap inputs; named fields at
// the dispatch site make same-typed argument transposition impossible.
type builderOptions struct {
	Addr          string
	Workspace     string
	SpoolDir      string
	StateURL      string
	ToolchainFile string
}

func runBuilder(o builderOptions) {
	tc, err := os.ReadFile(o.ToolchainFile)
	if err != nil {
		log.Fatalf("read toolchain id: %v", err)
	}
	toolchainID := strings.TrimSpace(string(tc))
	if toolchainID == "" {
		log.Fatalf("empty toolchain id in %s", o.ToolchainFile)
	}

	token := os.Getenv("STATE_TOKEN")
	if o.StateURL == "" || token == "" {
		log.Fatalf("builder needs STATE_URL and STATE_TOKEN: the state service owns durable logs/audit/status")
	}
	if err := os.MkdirAll(o.SpoolDir, 0o755); err != nil {
		log.Fatalf("create spool %s: %v", o.SpoolDir, err)
	}

	stateSink := sinkAdapter{sink.NewClient(o.StateURL, token)}
	srv := mcpserver.New(mcpserver.Config{
		Version:      version,
		ToolchainID:  toolchainID,
		Workspace:    o.Workspace,
		ManifestPath: filepath.Join(o.Workspace, manifest.DefaultPath),
		LogsDir:      o.SpoolDir,
		Runner: &runner.Runner{
			LogsDir:     o.SpoolDir,
			ToolchainID: toolchainID,
			Sink:        stateSink,
		},
		Audit:      stateSink.Client, // *sink.Client satisfies mcpserver.Auditor
		LogFetcher: stateSink.Client, // ...and mcpserver.LogFetcher
	})
	serveHTTP(&http.Server{Addr: o.Addr, Handler: srv.Handler()},
		fmt.Sprintf("mcp (toolchain %s → state %s)", toolchainID, o.StateURL))
}

// sinkAdapter adapts *sink.Client to runner.Sink: the embedded client
// supplies Reupload/Finalize/PutStatus directly; only StartRun needs the
// interface return type (io.WriteCloser instead of the concrete *LogStream).
type sinkAdapter struct{ *sink.Client }

func (s sinkAdapter) StartRun(id runid.ID) io.WriteCloser { return s.Client.StartRun(id) }
