package scholar

import (
	"fmt"
	"net"
	"time"
)

// defaultEgressProbes are stable public anycast IPs used ONLY to prove the
// absence of egress.  They are not liveness targets.
var defaultEgressProbes = []string{"1.1.1.1:443", "8.8.8.8:53"}

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
