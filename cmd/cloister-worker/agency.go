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

	"github.com/jeffbstewart/cloister/internal/agency"
)

// agencyRole parses the agency's flag set and returns its bootstrap.
func agencyRole(args []string) (func(), error) {
	fs := flag.NewFlagSet("agency", flag.ContinueOnError)
	common := registerCommon(fs, ":11434")
	upstream := fs.String("upstream", "http://infer:11434",
		"base URL of the model server a pass-through door fronts")
	configPath := fs.String("config", "",
		"path to the engine-class routing config (YAML); when set, the door routes classes instead of passing through")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	// The two modes are a deliberate startup choice: naming both is a
	// contradiction, refused rather than resolved by precedence.
	if *configPath != "" {
		explicitUpstream := false
		fs.Visit(func(f *flag.Flag) { explicitUpstream = explicitUpstream || f.Name == "upstream" })
		if explicitUpstream {
			return nil, fmt.Errorf("agency: -config and -upstream are mutually exclusive: choose class routing or pass-through")
		}
	}
	return common.runOrProbe(func() {
		runAgency(agencyOptions{Addr: *common.addr, Upstream: *upstream, ConfigPath: *configPath})
	}), nil
}

// agencyOptions carries the agency's bootstrap inputs.
type agencyOptions struct {
	Addr       string
	Upstream   string
	ConfigPath string
}

func runAgency(o agencyOptions) {
	cfg := agency.Config{}
	label := fmt.Sprintf("agency (upstream %s)", o.Upstream)
	if o.ConfigPath != "" {
		routes, err := agency.LoadRouterConfig(o.ConfigPath)
		if err != nil {
			log.Fatalf("agency: %v", err)
		}
		cfg.Routes = routes
		label = fmt.Sprintf("agency (routing config %s)", o.ConfigPath)
	} else {
		cfg.UpstreamURL = o.Upstream
	}
	srv, err := agency.New(cfg)
	if err != nil {
		log.Fatalf("agency: %v", err)
	}
	// Node presence probes ride the process lifetime: a no-op in
	// pass-through mode, and the goroutine dies with the process.
	go srv.ProbePresence(context.Background())
	serveHTTP(&http.Server{Addr: o.Addr, Handler: srv.Handler()}, label)
}
