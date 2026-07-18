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
	"github.com/jeffbstewart/cloister/internal/warming"
)

// builderRole parses the builder's flag set and returns its bootstrap.
func builderRole(args []string) (func(), error) {
	fs := flag.NewFlagSet("builder", flag.ContinueOnError)
	common := registerCommon(fs, ":9200")
	workspace := fs.String("workspace", "/workspace", "project bind mount; actions run here")
	spoolDir := fs.String("spool", "/spool", "local (tmpfs) log spool for digests and get_log")
	stateURL := fs.String("state-url", envOr("STATE_URL", ""), "base URL of the state service")
	toolchainFile := fs.String("toolchain-file", "/etc/cloister-worker/toolchain",
		"file holding this image's toolchain id")
	markWarmed := fs.Bool("mark-warmed", false,
		"record that the airlock warmed this toolchain's cache, then exit (run via docker exec by the warming script)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if *markWarmed {
		return func() {
			id, err := readToolchainID(*toolchainFile)
			if err != nil {
				log.Fatalf("mark-warmed: %v", err)
			}
			path, err := warmingConfig(id).Mark()
			if err != nil {
				log.Fatalf("mark-warmed: %v", err)
			}
			log.Printf("recorded toolchain %s warming at %s", id, path)
		}, nil
	}
	return common.runOrProbe(func() {
		runBuilder(builderOptions{
			Addr: *common.addr, Workspace: *workspace, SpoolDir: *spoolDir,
			StateURL: *stateURL, ToolchainFile: *toolchainFile,
		})
	}), nil
}

// readToolchainID loads the id the toolchain image baked; the builder has
// no identity without one.
func readToolchainID(path string) (string, error) {
	tc, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read toolchain id: %w", err)
	}
	id := strings.TrimSpace(string(tc))
	if id == "" {
		return "", fmt.Errorf("empty toolchain id in %s", path)
	}
	return id, nil
}

// warmingConfig locates the warming handshake for this toolchain: the
// image-baked instructions and the marker in the per-user cache home
// (HOME rides the BUILD_HOME bind, so the marker lives beside the caches
// it vouches for).
func warmingConfig(toolchainID string) warming.Config {
	return warming.Config{
		InstructionsPath: warming.DefaultInstructionsPath,
		CacheHome:        envOr("HOME", "/home/build"),
		ToolchainID:      toolchainID,
	}
}

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
	toolchainID, err := readToolchainID(o.ToolchainFile)
	if err != nil {
		log.Fatal(err)
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
		WarmCheck:  warmingConfig(toolchainID).Check,
	})
	serveHTTP(&http.Server{Addr: o.Addr, Handler: srv.Handler()},
		fmt.Sprintf("mcp (toolchain %s → state %s)", toolchainID, o.StateURL))
}

// sinkAdapter adapts *sink.Client to runner.Sink: the embedded client
// supplies Reupload/Finalize/PutStatus directly; only StartRun needs the
// interface return type (io.WriteCloser instead of the concrete *LogStream).
type sinkAdapter struct{ *sink.Client }

func (s sinkAdapter) StartRun(id runid.ID) io.WriteCloser { return s.Client.StartRun(id) }
