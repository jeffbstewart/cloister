package scholar

import (
	"net"
	"testing"
	"time"
)

// TestSelfCheckDetectsEgress: a reachable probe (a live local listener) means
// egress exists → the self-check must fail (refuse to start).
func TestSelfCheckDetectsEgress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := assertNoPublicEgress([]string{ln.Addr().String()}, time.Second); err == nil {
		t.Error("want an error when a probe connects (egress present)")
	}
}

// TestSelfCheckPassesWhenContained: an unroutable TEST-NET-1 address (RFC 5737)
// won't connect → contained → nil.  Negative-only, no liveness assertion.
func TestSelfCheckPassesWhenContained(t *testing.T) {
	if err := assertNoPublicEgress([]string{"192.0.2.1:443"}, 500*time.Millisecond); err != nil {
		t.Errorf("want nil when probes fail to connect, got %v", err)
	}
}
