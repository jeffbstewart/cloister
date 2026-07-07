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
	"log"
	"net/http"
	"os"

	"github.com/jeffbstewart/cloister/internal/status/sink"
)

// stateOptions carries the state service's bootstrap inputs.
type stateOptions struct {
	Addr     string
	StateDir string
}

func runStateService(o stateOptions) {
	token := os.Getenv("STATE_TOKEN")
	srv, err := sink.New(sink.Config{
		StateDir: o.StateDir,
		Token:    token,
		Version:  version,
	})
	if err != nil {
		log.Fatalf("state service: %v", err)
	}
	defer srv.Close()
	serveHTTP(&http.Server{Addr: o.Addr, Handler: srv.Handler()},
		fmt.Sprintf("state service (state %s)", o.StateDir))
}
