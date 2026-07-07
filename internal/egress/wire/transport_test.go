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

package wire

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewGuardedClientRejectsBadRelayAddr(t *testing.T) {
	bad := []map[string]string{
		{"kagi.com": "no-port"},
		{"kagi.com": ":8443"},   // empty host
		{"kagi.com": "host:"},   // empty port
		{"": "kagi-relay:8443"}, // empty upstream
	}
	for _, relays := range bad {
		if _, err := NewGuardedClient(relays, time.Second); err == nil {
			t.Errorf("NewGuardedClient(%v) accepted a bad relay address", relays)
		}
	}
}

func TestGuardedClientRoutesMappedAndRefusesUnmapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	relayAddr := strings.TrimPrefix(srv.URL, "http://") // 127.0.0.1:port

	hc, err := NewGuardedClient(map[string]string{"upstream.test": relayAddr}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	// Mapped host → dialed to the relay (the stub server), 200.
	resp, err := hc.Get("http://upstream.test/")
	if err != nil {
		t.Fatalf("mapped host request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("mapped host status = %d", resp.StatusCode)
	}

	// Unmapped host → refused at dial time.
	if _, err := hc.Get("http://other.test/"); err == nil || !strings.Contains(err.Error(), "refusing to dial") {
		t.Errorf("unmapped host err = %v, want a refusal", err)
	}
}
