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
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // embed zone data so TZ=<zone> localizes status pages in any base image
)

// commonFlags are the flags every role shares: where to listen, and the
// container HEALTHCHECK probe (registered per role so `scribe -healthcheck`
// probes the scribe's default port).
type commonFlags struct {
	addr        *string
	healthcheck *bool
}

// registerCommon registers the shared flags on a role's flag set, with the
// role's own default listen address.
func registerCommon(fs *flag.FlagSet, defaultAddr string) commonFlags {
	return commonFlags{
		addr: fs.String("addr", defaultAddr, "listen address"),
		healthcheck: fs.Bool("healthcheck", false,
			"probe the local /healthz and exit 0/1 (container HEALTHCHECK)"),
	}
}

// runOrProbe wraps a role's action: when -healthcheck was passed, the
// process probes the running server and exits instead of serving.
func (c commonFlags) runOrProbe(run func()) func() {
	if *c.healthcheck {
		addr := *c.addr
		return func() { os.Exit(probeHealthz(addr)) }
	}
	return run
}

// serveHTTP runs the server until SIGTERM/SIGINT, then drains
// connections and returns nil; a serve failure returns the error so the
// caller can release its resources before exiting — log.Fatalf here
// would skip the caller's defers (a crash-looping container would leak
// whatever the role holds on disk, e.g. the archivist's hooks dir).
// roleName is the resolved role, set by main once dispatch picks one; it
// is what the startup banner calls this process.
var roleName string

// bannerName is the role for the banner, falling back to the binary's own
// name when nothing resolved a role (a direct serveHTTP call in a test).
func bannerName() string {
	if roleName != "" {
		return roleName
	}
	return "cloister-worker"
}

func serveHTTP(httpSrv *http.Server, what string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()
	log.Printf("%s %s serving %s at %s", bannerName(), version, what, httpSrv.Addr)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Print("signal received; shutting down")
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shCtx)
		return nil
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
