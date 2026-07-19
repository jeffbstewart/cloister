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
	"net/http"
	"os"

	"github.com/jeffbstewart/cloister/internal/agency"
)

// agencyRole parses the agency's flag set and returns its bootstrap.
// Class routing is the DEFAULT mode: the routing table comes from -config,
// else the AGENCY_ROUTES env (a host-local override the operator mounts),
// else the config embedded in the binary.  Pass-through survives only as an
// explicit escape hatch: it requires -upstream on the command line.
func agencyRole(args []string) (func(), error) {
	fs := flag.NewFlagSet("agency", flag.ContinueOnError)
	common := registerCommon(fs, ":11434")
	upstream := fs.String("upstream", "http://infer:11434",
		"EXPLICIT pass-through mode: front this single model server instead of routing classes")
	configPath := fs.String("config", "",
		"path to an engine-class routing config (YAML); default: $AGENCY_ROUTES, else the override mounted at "+agencyRoutesOverridePath+", else the embedded reference config")
	statusDir := fs.String("status-dir", "",
		"directory (the agency_status volume mount) for atomic status snapshots; routing mode only")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	passThrough := false
	fs.Visit(func(f *flag.Flag) { passThrough = passThrough || f.Name == "upstream" })
	// The two modes are a deliberate startup choice: naming both is a
	// contradiction, refused rather than resolved by precedence.
	if passThrough && *configPath != "" {
		return nil, fmt.Errorf("agency: -config and -upstream are mutually exclusive: choose class routing or pass-through")
	}
	if passThrough && *statusDir != "" {
		return nil, fmt.Errorf("agency: -status-dir is incompatible with -upstream: a pass-through door has no status to publish")
	}
	return common.runOrProbe(func() {
		runAgency(agencyOptions{
			Addr:        *common.addr,
			PassThrough: passThrough,
			Upstream:    *upstream,
			ConfigPath:  *configPath,
			StatusDir:   *statusDir,
		})
	}), nil
}

// agencyOptions carries the agency's bootstrap inputs.
type agencyOptions struct {
	Addr        string
	PassThrough bool
	Upstream    string
	ConfigPath  string
	StatusDir   string
}

// agencyRoutesOverridePath is the well-known mount point of the compose
// stack's optional routing override.  The compose file always declares the
// mount, defaulting the host side to /dev/null so no compose edit is ever
// needed: a REGULAR file here is an operator's override, the /dev/null
// character device means none was given.
const agencyRoutesOverridePath = "/etc/agency/routes.yaml"

// resolveRoutesPath picks where the routing table comes from: the -config
// flag, the AGENCY_ROUTES env (local runs), the override mount, or — empty
// path — the embedded default.  A directory at the override mount is a
// startup error, not a silent fallback: it means the operator pointed the
// stack var at a directory instead of the routes file.
func resolveRoutesPath(flagPath string) (path, source string, err error) {
	if flagPath != "" {
		return flagPath, "-config", nil
	}
	if env := os.Getenv("AGENCY_ROUTES"); env != "" {
		return env, "$AGENCY_ROUTES", nil
	}
	fi, statErr := os.Stat(agencyRoutesOverridePath)
	switch {
	case statErr != nil:
		return "", "", nil // no override mount at all: embedded default
	case fi.Mode().IsRegular():
		return agencyRoutesOverridePath, "override mount", nil
	case fi.IsDir():
		return "", "", fmt.Errorf("agency: %s is a directory — AGENCY_ROUTES must name the routes FILE on the host", agencyRoutesOverridePath)
	default:
		return "", "", nil // the /dev/null placeholder: embedded default
	}
}

func runAgency(o agencyOptions) {
	cfg := agency.Config{}
	var label string
	switch {
	case o.PassThrough:
		cfg.UpstreamURL = o.Upstream
		label = fmt.Sprintf("agency (pass-through, upstream %s)", o.Upstream)
	default:
		path, source, err := resolveRoutesPath(o.ConfigPath)
		if err != nil {
			log.Fatalf("%v", err)
		}
		var routes *agency.RouterConfig
		if path != "" {
			routes, err = agency.LoadRouterConfig(path)
			label = fmt.Sprintf("agency (routing config %s, via %s)", path, source)
		} else {
			routes, err = agency.DefaultRouterConfig()
			label = "agency (embedded default routing config)"
		}
		if err != nil {
			log.Fatalf("agency: %v", err)
		}
		cfg.Routes = routes
	}
	srv, err := agency.New(cfg)
	if err != nil {
		log.Fatalf("agency: %v", err)
	}
	// Presence probes and status snapshots ride the process lifetime:
	// no-ops in pass-through mode, and the goroutines die with the process.
	go srv.ProbePresence(context.Background())
	if o.StatusDir != "" {
		go srv.WriteStatusSnapshots(context.Background(), o.StatusDir)
	}
	serveHTTP(&http.Server{Addr: o.Addr, Handler: srv.Handler()}, label)
}
