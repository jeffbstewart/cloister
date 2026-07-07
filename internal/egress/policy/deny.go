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

package policy

import (
	"fmt"
	"log"
	"net/netip"
	"regexp"
	"strings"
)

// normalizeHost folds a host for matching: lowercase, trailing dot stripped.
//
// NOTE (punycode, deferred): full IDNA/punycode canonicalization needs
// golang.org/x/net/idna, a new dependency (a new dependency).  The curated deny list
// is ASCII and technical research targets are overwhelmingly ASCII, so Phase 0
// folds ASCII case only; a Unicode host is compared in its lowercased byte form.
// Adopting idna for homograph-proof matching is a follow-up dependency decision.
func normalizeHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}

// denyHostRE accepts an exact dotted host or a single leading "*." wildcard,
// each label being a normal DNS label.  It runs AFTER normalizeHost.
var denyHostRE = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// validateDenyHost checks that a deny entry is a well-formed host or "*." rule
// (referenced by Policy.validate).  It rejects embedded or trailing wildcards,
// schemes, ports, and paths — a deny entry is a host pattern, nothing more.
func validateDenyHost(h string) error {
	n := normalizeHost(h)
	if strings.Contains(n, "*") && !strings.HasPrefix(n, "*.") {
		return fmt.Errorf("a wildcard is allowed only as a single leading %q", "*.")
	}
	if strings.Count(n, "*") > 1 {
		return fmt.Errorf("at most one %q wildcard", "*.")
	}
	if !denyHostRE.MatchString(n) {
		return fmt.Errorf("not a valid host or %q wildcard pattern", "*.")
	}
	return nil
}

// Denies reports whether extracting (host, path) is forbidden by the policy.
// Host matching is case-insensitive; the path is compared byte-for-byte (URL
// paths are case-sensitive).  A "*." entry matches subdomains, not the
// apex — add the apex as its own entry to deny it too.
func (p *Policy) Denies(host, path string) bool {
	host = normalizeHost(host)
	// Immediate operator-visible signal on a match; the durable rejected_denied
	// audit record is emitted at the subsystem layer.
	if d, ok := matchDeny(p.Extract.Deny, host, path); ok {
		log.Printf("egress: deny — host %q blocked by deny rule %q", host, d.Host)
		return true
	}
	if p.Search.DenySearchEnginePages != nil && *p.Search.DenySearchEnginePages {
		if d, ok := matchDeny(builtinSearchEngineDeny, host, path); ok {
			log.Printf("egress: deny — host %q blocked by built-in search-engine rule %q (denySearchEnginePages)", host, d.Host)
			return true
		}
	}
	return false
}

// matchDeny reports the first entry in list that matches (host, path), if any.
func matchDeny(list []DenyEntry, host, path string) (DenyEntry, bool) {
	for _, d := range list {
		if hostPatternMatch(normalizeHost(d.Host), host) {
			if d.PathPrefix == "" || strings.HasPrefix(path, d.PathPrefix) {
				return d, true
			}
		}
	}
	return DenyEntry{}, false
}

// hostPatternMatch matches a normalized host against a normalized deny pattern.
func hostPatternMatch(pattern, host string) bool {
	if suffix, ok := strings.CutPrefix(pattern, "*."); ok {
		dotted := "." + suffix // "*.example.com" matches x.example.com, not example.com
		return strings.HasSuffix(host, dotted) && len(host) > len(dotted)
	}
	return host == pattern
}

// IsInternalHost reports whether a host is obviously non-public and so must not
// be handed to Kagi's server-side fetch even as hygiene.  It refuses
// loopback / RFC-1918 / link-local / ULA / unspecified / multicast literals and
// the localhost/.local name families. (SSRF *into our cell* is already
// impossible — Kagi's cloud can't route to us — so this is belt, not the pin.)
func IsInternalHost(host string) bool {
	h := strings.Trim(normalizeHost(host), "[]")
	if h == "" || h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local") {
		return true
	}
	if addr, err := netip.ParseAddr(h); err == nil {
		return addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() ||
			addr.IsLinkLocalMulticast() || addr.IsUnspecified() || addr.IsMulticast()
	}
	return false
}
