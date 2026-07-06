package policy

// Built-in, code-maintained deny set for general search-engine result pages
// (SERPs).  Activated per cell by `search.denySearchEnginePages: true`, so
// an operator gets Google's ~180 ccTLDs and the other majors from one policy
// line instead of hand-listing them.  This is the single place to maintain that
// list.
//
// Why deny SERPs at all: a SERP URL is a query-string exfil channel
// (?q=<secret>), its content is attacker-influenceable (SEO poisoning), and
// extracting one is an uncontrolled second search path that bypasses our chosen
// engine.  This is DEFENSE-IN-DEPTH only: Kagi fetches server-side and follows
// redirects we cannot re-check, so a host that redirects into search (a ccTLD
// redirect, a URL shortener) slips a host-based deny regardless.  The real exfil
// gate is the per-retrieval operator approval on raw URLs, reinforced by the
// scholar's system-prompt discipline.  AI answer-engines (Perplexity, You.com,
// Phind) are left to that prompt discipline and operator discretion, not this
// list.

// googleCCTLDs are the domains that host Google web search on /search.  Google
// largely unified onto google.com (most ccTLDs now redirect), so this covers the
// major markets a model is realistically likely to name; add more here as
// needed — it's the canonical location.
var googleCCTLDs = []string{
	"com", "co.uk", "de", "fr", "es", "it", "nl", "be", "ch", "at",
	"se", "no", "dk", "fi", "pl", "cz", "pt", "gr", "ie", "ro",
	"hu", "sk", "bg", "hr", "lt", "lv", "ee", "si", "rs",
	"ca", "com.au", "co.nz", "co.in", "co.jp", "co.kr", "com.tw",
	"com.hk", "com.sg", "co.th", "com.ph", "com.vn", "co.id", "com.my",
	"com.br", "com.mx", "com.ar", "cl", "com.co", "com.pe", "com.ve",
	"ru", "com.ua", "by", "kz", "com.tr", "co.za", "com.eg", "com.sa",
	"ae", "com.ng", "co.ke",
}

// otherSearchEngineDeny covers the non-Google general engines.  Whole-host where
// search *is* the site (path-restrict can't catch a root-query SERP); a search
// path where the host also serves other content.
var otherSearchEngineDeny = []DenyEntry{
	{Host: "bing.com", PathPrefix: "/search"},
	{Host: "www.bing.com", PathPrefix: "/search"},
	{Host: "cn.bing.com", PathPrefix: "/search"},
	{Host: "duckduckgo.com"},
	{Host: "*.duckduckgo.com"},
	{Host: "search.yahoo.com"},
	{Host: "search.brave.com"}, // our own alternate engine's SERP, notably
	{Host: "startpage.com"},
	{Host: "*.startpage.com"},
	{Host: "mojeek.com"},
	{Host: "*.mojeek.com"},
	{Host: "ecosia.org", PathPrefix: "/search"},
	{Host: "www.ecosia.org", PathPrefix: "/search"},
	{Host: "yandex.com", PathPrefix: "/search"},
	{Host: "yandex.ru", PathPrefix: "/search"},
	{Host: "baidu.com", PathPrefix: "/s"},
	{Host: "www.baidu.com", PathPrefix: "/s"},
}

// builtinSearchEngineDeny is the assembled set, consulted by Policy.Denies when
// the toggle is on.
var builtinSearchEngineDeny = buildSearchEngineDeny()

func buildSearchEngineDeny() []DenyEntry {
	out := make([]DenyEntry, 0, len(googleCCTLDs)*2+len(otherSearchEngineDeny))
	for _, tld := range googleCCTLDs {
		out = append(out,
			DenyEntry{Host: "google." + tld, PathPrefix: "/search"},
			DenyEntry{Host: "www.google." + tld, PathPrefix: "/search"},
		)
	}
	return append(out, otherSearchEngineDeny...)
}
