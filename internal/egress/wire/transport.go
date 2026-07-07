// Package wire is the egress subsystem's shared transport leaf: the ONE
// guarded HTTP client constructor, size-capped GET/POST helpers, and the
// secret scrubber.  The search and extract providers build on wire without
// importing the core egress package, which keeps the dependency graph
// acyclic.
package wire

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// NewGuardedClient is the ONE constructor for the egress subsystem's outbound
// HTTP.  Its transport can dial only the hosts in relays — the Kagi
// endpoint (and Brave, if enabled) — and each is redirected to its socat relay
// address; any other host is refused at dial time, so no bare/arbitrary egress
// is possible even by accident.  Proxy is nil explicitly, so a stray HTTP(S)_PROXY
// env var cannot tunnel around the relay.
//
// TLS is end-to-end: TLSClientConfig is left nil, so the transport derives the
// SNI and validates the certificate against the REQUEST host (e.g. kagi.com),
// not the relay it actually dialed.  The relay only ever sees ciphertext.
//
// relays maps an upstream host (no port) to a relay "host:port".  The relay
// addresses are the ones we control, so they are validated HERE, at
// construction — a malformed relay target is a startup error, never a surprise
// at dial time.  The whole package's structural rule ("no http.DefaultClient /
// http.Get / bare http.Transport") funnels every request through here.
func NewGuardedClient(relays map[string]string, timeout time.Duration) (*http.Client, error) {
	m := make(map[string]string, len(relays))
	for host, addr := range relays {
		if host == "" {
			return nil, fmt.Errorf("egress: empty upstream host in relay map")
		}
		rh, rp, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("egress: relay address %q for %q is not host:port: %w", addr, host, err)
		}
		if rh == "" || rp == "" {
			return nil, fmt.Errorf("egress: relay address %q for %q needs both host and port", addr, host)
		}
		m[strings.ToLower(host)] = addr
	}
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy: nil, // never honor HTTP(S)_PROXY — the relay is the only way out
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// addr is the request's upstream host:port, supplied by the
			// transport from the request URL. We only look it up; a value that
			// matches no configured relay is refused (no bad-address parsing here).
			host, _, _ := net.SplitHostPort(addr)
			relay, ok := m[strings.ToLower(host)]
			if !ok {
				return nil, fmt.Errorf("egress: refusing to dial %q — only configured relays are reachable", addr)
			}
			return d.DialContext(ctx, network, relay)
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{Transport: tr, Timeout: timeout}, nil
}
