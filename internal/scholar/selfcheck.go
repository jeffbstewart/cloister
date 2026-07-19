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

package scholar

import (
	"context"
	"fmt"
	"net"
	"time"
)

// defaultEgressProbes are stable public anycast IPs used ONLY to prove the
// absence of egress.  They are not liveness targets.
var defaultEgressProbes = []string{"1.1.1.1:443", "8.8.8.8:53"}

// defaultDNSProbes are well-known public names used ONLY to prove that
// external name resolution is dead.  Resolution SUCCESS is the failure.
var defaultDNSProbes = []string{"cloudflare.com", "google.com"}

// AssertNoPublicEgress is the fail-closed boot self-check: it tries to
// TCP-connect to fixed public IPs and returns an error if ANY connects — the
// scholar must have no route to the arbitrary internet (only its relay).  It is
// NEGATIVE-ONLY: a connect failure is the expected, contained result.  It never
// verifies that the relay, Kagi, or the model endpoint is reachable — that is
// liveness, surfaced by a failing research call, not a start-time gate (do not
// confuse uptime monitoring with a start constraint).
func AssertNoPublicEgress() error {
	return assertNoPublicEgress(defaultEgressProbes, 3*time.Second)
}

func assertNoPublicEgress(probes []string, timeout time.Duration) error {
	for _, addr := range probes {
		conn, err := net.DialTimeout("tcp", addr, timeout)
		if err == nil {
			_ = conn.Close()
			return fmt.Errorf("egress self-check FAILED: reached public %s — the scholar must have no route to the arbitrary internet (only the relay); refusing to start", addr)
		}
	}
	return nil
}

// AssertNoExternalDNS is the DNS half of the fail-closed boot self-check.
// The TCP probe proves no packet route out; this proves the embedded
// resolver's upstream is dead (`dns: 127.0.0.1` in the compose file) — on
// engines with daemon-side DNS forwarding (CVE-2024-29018), name
// resolution alone exfiltrates from an internal network, so the two paths
// must be probed independently.  Like the TCP probe it is NEGATIVE-ONLY:
// a lookup failure is the expected, contained result.
func AssertNoExternalDNS() error {
	return assertNoExternalDNS(defaultDNSProbes, 3*time.Second, net.DefaultResolver.LookupHost)
}

func assertNoExternalDNS(names []string, timeout time.Duration, lookup func(context.Context, string) ([]string, error)) error {
	for _, name := range names {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		addrs, err := lookup(ctx, name)
		cancel()
		if err == nil && len(addrs) > 0 {
			return fmt.Errorf("dns self-check FAILED: resolved public name %q (%s) — external DNS must be dead (`dns: 127.0.0.1`); refusing to start", name, addrs[0])
		}
	}
	return nil
}
