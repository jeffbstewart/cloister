package egress

import (
	"fmt"

	"github.com/jeffbstewart/cloister/internal/egress/extract"
	"github.com/jeffbstewart/cloister/internal/egress/policy"
	"github.com/jeffbstewart/cloister/internal/egress/search"
	"github.com/jeffbstewart/cloister/internal/egress/wire"
)

// Upstream hosts.  These are the real endpoints; the guarded client routes
// each to its socat relay, and TLS validates against these names.
const (
	kagiHost  = "kagi.com"
	braveHost = "api.search.brave.com"
	kagiBase  = "https://kagi.com"
	braveBase = "https://api.search.brave.com"
)

// ProviderOptions configures NewProviders.  Relays are the socat relay
// addresses ("host:port"); NewProviders maps them to the known upstream hosts,
// so the wiring layer never needs the upstream hostnames.  Kagi fields are always
// required (extract is Kagi-only, even for a Brave-search cell); Brave fields are
// required only when the policy selects search.engine: brave.
type ProviderOptions struct {
	Policy    *policy.Policy
	KagiKey   string
	KagiRelay string // socat relay address as "host:port", e.g. "kagi-relay:8443"
	BraveKey  string
	// BraveRelay is the relay for api.search.brave.com, same "host:port" form,
	// e.g. "brave-relay:8443".  Required only when search.engine: brave.
	BraveRelay string
}

// NewProviders builds the guarded HTTP client and the configured Searcher +
// Retriever for production wiring.  Fail-closed: a missing key or relay is a
// startup error, never a surprise at dial time.
func NewProviders(opts ProviderOptions) (Searcher, Retriever, error) {
	p := opts.Policy
	if p == nil {
		return nil, nil, fmt.Errorf("egress: NewProviders: Policy is required")
	}
	if opts.KagiKey == "" {
		return nil, nil, fmt.Errorf("egress: KAGI_API_KEY is required (Kagi extract has no alternate)")
	}
	if opts.KagiRelay == "" {
		return nil, nil, fmt.Errorf("egress: KAGI_RELAY_ADDR is required")
	}
	relays := map[string]string{kagiHost: opts.KagiRelay}
	if p.Search.Engine == policy.EngineBrave {
		if opts.BraveKey == "" {
			return nil, nil, fmt.Errorf("egress: BRAVE_API_KEY is required for search.engine: brave")
		}
		if opts.BraveRelay == "" {
			return nil, nil, fmt.Errorf("egress: BRAVE_RELAY_ADDR is required for search.engine: brave")
		}
		relays[braveHost] = opts.BraveRelay
	}
	timeout := p.Limits.Timeout.Std()
	hc, err := wire.NewGuardedClient(relays, timeout)
	if err != nil {
		return nil, nil, err
	}
	max := p.Limits.MaxResponseBytes
	retr := extract.NewKagiRetriever(kagiBase, opts.KagiKey, hc, max, timeout)

	switch p.Search.Engine {
	case policy.EngineKagi:
		return search.NewKagiSearcher(kagiBase, opts.KagiKey, hc, max), retr, nil
	case policy.EngineBrave:
		return search.NewBraveSearcher(braveBase, opts.BraveKey, hc, max), retr, nil
	default:
		return nil, nil, fmt.Errorf("egress: unknown engine %q", p.Search.Engine)
	}
}
