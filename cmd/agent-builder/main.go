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

// agent-builder is the one binary of the Cloister cell — every worker is a
// mode of it, selected by the REQUIRED -worker-mode flag:
//
//	builder        reads /workspace/agent-harness.yaml, serves the declared
//	               actions as MCP tools on :9200, and streams
//	               logs/audit/status to the state service.  Holds NO /state
//	               mount — agent-authored build code cannot touch the record
//	               of what it did.
//	state-service  owns the /state volume, accepts the workers'
//	               authenticated appends, and serves the read-only status
//	               pages on :9201.  No egress; reachable from the host only
//	               via the socat status relay.
//	scribe         the workspace editor: the sole audited writer of
//	               /workspace, serving confined write ops as MCP tools on
//	               :9300 and auditing each to the state service.
//	scholar        the quarantined web-research agent: one research MCP
//	               tool, reaching the web only through its pinned relay,
//	               refusing to start if any other route exists.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // embed zone data so TZ=<zone> localizes status pages in any base image

	"github.com/jeffbstewart/cloister/internal/egress"
	"github.com/jeffbstewart/cloister/internal/egress/policy"
	"github.com/jeffbstewart/cloister/internal/egress/wire"
	"github.com/jeffbstewart/cloister/internal/manifest"
	"github.com/jeffbstewart/cloister/internal/mcpserver"
	"github.com/jeffbstewart/cloister/internal/runid"
	"github.com/jeffbstewart/cloister/internal/runner"
	"github.com/jeffbstewart/cloister/internal/scholar"
	"github.com/jeffbstewart/cloister/internal/scribe"
	"github.com/jeffbstewart/cloister/internal/status/sink"
	"github.com/jeffbstewart/cloister/internal/workspace"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

// workerMode selects which worker this process runs as.  There is
// deliberately no default: a cell's compose file must say what each
// container is, and the incompatible per-mode flags cannot be combined.
type workerMode string

const (
	modeBuilder      workerMode = "builder"
	modeStateService workerMode = "state-service"
	modeScribe       workerMode = "scribe"
	modeScholar      workerMode = "scholar"
)

const workerModes = "builder | state-service | scribe | scholar"

func main() {
	mode := flag.String("worker-mode", "", "REQUIRED: which worker this process is — "+workerModes)
	addr := flag.String("addr", ":9200", "listen address")
	workspace := flag.String("workspace", "/workspace", "project bind mount; actions run here")
	spoolDir := flag.String("spool", "/spool", "local (tmpfs) log spool for digests and get_log")
	stateDir := flag.String("state-dir", "/state", "state-service volume: logs, audit, status")
	stateURL := flag.String("state-url", envOr("STATE_URL", ""), "builder: base URL of the state service")
	toolchainFile := flag.String("toolchain-file", "/etc/agent-builder/toolchain",
		"file holding this image's toolchain id")
	scribeStageDir := flag.String("scribe-stage-dir", "/scribe-state",
		"scribe: durable staging dir for pending (approval-gated) changes")
	scribeApprovals := flag.Bool("scribe-approvals", false,
		"scribe: hold gated writes PENDING human approval instead of refusing them")
	scholarPolicy := flag.String("policy", "/etc/scholar/policy.yaml",
		"scholar: read-only egress policy file")
	burnDir := flag.String("burn-dir", "/burn",
		"scholar: writable volume for the burn-rate ledger (timestamps only)")
	answerGate := flag.Bool("answer-gate", true,
		"scholar: gate the answer on operator approval before returning it")
	healthcheck := flag.Bool("healthcheck", false,
		"probe the local /healthz and exit 0/1 (container HEALTHCHECK)")
	flag.Parse()

	if *healthcheck {
		os.Exit(probeHealthz(*addr))
	}

	log.SetFlags(log.LstdFlags | log.LUTC)

	switch workerMode(*mode) {
	case modeBuilder:
		runBuilder(*addr, *workspace, *spoolDir, *stateURL, *toolchainFile)
	case modeStateService:
		runStateService(*addr, *stateDir)
	case modeScribe:
		runScribe(*addr, *workspace, *stateURL, *scribeStageDir, *scribeApprovals)
	case modeScholar:
		runScholar(*addr, *scholarPolicy, *burnDir, *stateURL, *answerGate)
	case "":
		log.Fatalf("-worker-mode is required: %s", workerModes)
	default:
		log.Fatalf("unknown -worker-mode %q: want %s", *mode, workerModes)
	}
}

func runScholar(addr, policyPath, burnDir, stateURL string, answerGate bool) {
	// The fail-closed egress self-check runs FIRST: if the scholar can
	// reach the arbitrary internet, it must not start.
	if err := scholar.AssertNoPublicEgress(); err != nil {
		log.Fatalf("scholar: %v", err)
	}
	log.Printf("scholar: egress self-check passed — no arbitrary internet route (relay=%s)", os.Getenv("KAGI_RELAY_ADDR"))

	token := os.Getenv("STATE_TOKEN")
	if stateURL == "" || token == "" {
		log.Fatalf("scholar needs STATE_URL and STATE_TOKEN: it audits every search/extract to the state service")
	}
	baseURL := envOr("OPENAI_BASE_URL", "")
	model := os.Getenv("OPENAI_MODEL")
	if baseURL == "" || model == "" {
		log.Fatalf("scholar needs OPENAI_BASE_URL and OPENAI_MODEL")
	}

	p, err := policy.LoadPolicy(policyPath)
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
	searchLedger, err := egress.OpenLedger(filepath.Join(burnDir, "search"), 48*time.Hour, now)
	if err != nil {
		log.Fatalf("scholar: %v", err)
	}
	extractLedger, err := egress.OpenLedger(filepath.Join(burnDir, "extract"), 48*time.Hour, now)
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

	stateClient := sink.NewClient(stateURL, token) // one client: audit + approvals + transcripts
	srv := scholar.New(scholar.Config{
		Version: version,
		Egress:  sub,
		Model: scholar.NewModelClient(scholar.ModelOptions{
			BaseURL: baseURL,
			Model:   model,
			Key:     os.Getenv("OPENAI_API_KEY"),
		}),
		Audit:       stateClient,
		Approvals:   stateClient,
		Transcripts: stateClient,
		AnswerGate:  answerGate,
		Caps:        scholar.DefaultCaps(),
	})
	serveHTTP(&http.Server{Addr: addr, Handler: srv.Handler()},
		fmt.Sprintf("scholar (research → model %s @ %s, engine %s)", model, baseURL, sub.Engine()))
}

func runScribe(addr, workspaceDir, stateURL, stageDir string, approvals bool) {
	token := os.Getenv("STATE_TOKEN")
	if stateURL == "" || token == "" {
		log.Fatalf("scribe needs STATE_URL and STATE_TOKEN: it audits every mutation to the state service")
	}
	root, err := workspace.Open(workspaceDir)
	if err != nil {
		log.Fatalf("scribe: %v", err)
	}
	// One client satisfies Auditor, DiffStore, and ApprovalClient.
	client := sink.NewClient(stateURL, token)
	cfg := scribe.Config{
		Version:  version,
		Root:     root,
		Audit:    client,
		Diffs:    client,
		StageDir: stageDir,
	}
	if approvals {
		cfg.Approvals = client // hold gated writes pending approval (resolved on the /approvals page)
	}
	srv := scribe.New(cfg)
	srv.Recover() // resume any approvals staged before a restart (no-op when approvals are off)
	serveHTTP(&http.Server{Addr: addr, Handler: srv.Handler()},
		fmt.Sprintf("scribe (workspace %s → state %s)", workspaceDir, stateURL))
}

func runStateService(addr, stateDir string) {
	token := os.Getenv("STATE_TOKEN")
	srv, err := sink.New(sink.Config{
		StateDir: stateDir,
		Token:    token,
		Version:  version,
	})
	if err != nil {
		log.Fatalf("state service: %v", err)
	}
	defer srv.Close()
	serveHTTP(&http.Server{Addr: addr, Handler: srv.Handler()},
		fmt.Sprintf("state service (state %s)", stateDir))
}

func runBuilder(addr, workspace, spoolDir, stateURL, toolchainFile string) {
	tc, err := os.ReadFile(toolchainFile)
	if err != nil {
		log.Fatalf("read toolchain id: %v", err)
	}
	toolchainID := strings.TrimSpace(string(tc))
	if toolchainID == "" {
		log.Fatalf("empty toolchain id in %s", toolchainFile)
	}

	token := os.Getenv("STATE_TOKEN")
	if stateURL == "" || token == "" {
		log.Fatalf("builder needs STATE_URL and STATE_TOKEN: the state service owns durable logs/audit/status")
	}
	if err := os.MkdirAll(spoolDir, 0o755); err != nil {
		log.Fatalf("create spool %s: %v", spoolDir, err)
	}

	stateSink := sinkAdapter{sink.NewClient(stateURL, token)}
	srv := mcpserver.New(mcpserver.Config{
		Version:      version,
		ToolchainID:  toolchainID,
		Workspace:    workspace,
		ManifestPath: filepath.Join(workspace, manifest.DefaultPath),
		LogsDir:      spoolDir,
		Runner: &runner.Runner{
			LogsDir:     spoolDir,
			ToolchainID: toolchainID,
			Sink:        stateSink,
		},
		Audit:      stateSink.Client, // *sink.Client satisfies mcpserver.Auditor
		LogFetcher: stateSink.Client, // ...and mcpserver.LogFetcher
	})
	serveHTTP(&http.Server{Addr: addr, Handler: srv.Handler()},
		fmt.Sprintf("mcp (toolchain %s → state %s)", toolchainID, stateURL))
}

// sinkAdapter adapts *sink.Client to runner.Sink: the embedded client
// supplies Reupload/Finalize/PutStatus directly; only StartRun needs the
// interface return type (io.WriteCloser instead of the concrete *LogStream).
type sinkAdapter struct{ *sink.Client }

func (s sinkAdapter) StartRun(id runid.ID) io.WriteCloser { return s.Client.StartRun(id) }

// serveHTTP runs the server until SIGTERM/SIGINT, then drains connections.
func serveHTTP(httpSrv *http.Server, what string) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()
	log.Printf("agent-builder %s serving %s at %s", version, what, httpSrv.Addr)

	select {
	case err := <-errCh:
		log.Fatalf("serve: %v", err)
	case <-ctx.Done():
		log.Print("signal received; shutting down")
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shCtx)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// probeHealthz hits the running server's /healthz from inside the
// container, so the image needs no curl/wget for its HEALTHCHECK.
func probeHealthz(addr string) int {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthz: %s\n", resp.Status)
		return 1
	}
	return 0
}
