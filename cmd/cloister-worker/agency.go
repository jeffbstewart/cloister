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

	"github.com/jeffbstewart/cloister/internal/agency"
)

// agencyRole parses the agency's flag set and returns its bootstrap.
func agencyRole(args []string) (func(), error) {
	fs := flag.NewFlagSet("agency", flag.ContinueOnError)
	common := registerCommon(fs, ":11434")
	upstream := fs.String("upstream", "http://infer:11434",
		"base URL of the model server the inference door fronts")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return common.runOrProbe(func() {
		runAgency(agencyOptions{Addr: *common.addr, Upstream: *upstream})
	}), nil
}

// agencyOptions carries the agency's bootstrap inputs.
type agencyOptions struct {
	Addr     string
	Upstream string
}

func runAgency(o agencyOptions) {
	srv, err := agency.New(agency.Config{UpstreamURL: o.Upstream})
	if err != nil {
		log.Fatalf("agency: %v", err)
	}
	serveHTTP(&http.Server{Addr: o.Addr, Handler: srv.Handler()},
		fmt.Sprintf("agency (upstream %s)", o.Upstream))
}
