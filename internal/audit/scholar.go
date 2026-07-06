package audit

// Bounds a scholar call can exhaust.  Daily quotas persist across research
// calls; per-invocation ceilings reset with each one.
const (
	LimitSearchesPerDay        Limit = "searches_per_day"        // daily egress quota (persistent)
	LimitExtractsPerDay        Limit = "extracts_per_day"        // daily egress quota (persistent)
	LimitSearchesPerInvocation Limit = "searches_per_invocation" // one research call's ceiling
	LimitExtractsPerInvocation Limit = "extracts_per_invocation" // one research call's ceiling
	LimitQuerySize             Limit = "query_size"
	LimitTokens                Limit = "tokens"
	LimitSteps                 Limit = "steps"
)

// ResearchDetail is a scholar research-call's record body.  URLs and counts
// only — never page bodies or result content.
type ResearchDetail struct {
	Query            string `json:"query,omitempty"`
	Searches         int    `json:"searches,omitempty"`
	Extracts         int    `json:"extracts,omitempty"`
	Tokens           int    `json:"tokens,omitempty"`
	AnswerBytes      int    `json:"answerBytes,omitempty"`
	AnswerSHA256     string `json:"answerSha256,omitempty"`
	TranscriptStored bool   `json:"transcriptStored,omitempty"`
}

// SearchDetail is a scholar web_search record body.  The query and hit URLs
// — never the result snippets or page content.
type SearchDetail struct {
	Query      string   `json:"query"`
	Engine     string   `json:"engine,omitempty"`     // search backend
	ResultURLs []string `json:"resultUrls,omitempty"` // hit URLs
}

// ExtractDetail is a scholar extract_url_as_markdown record body: which URL
// was consulted and how it was targeted — never the fetched page body.  The
// opaque search handle is elided; a handle is resolved to its URL for the log.
type ExtractDetail struct {
	Via      string `json:"via,omitempty"`      // provenance: "search_result" | "raw_url"
	URL      string `json:"url,omitempty"`      // the URI retrieved (identity; present on success and failure)
	Provider string `json:"provider,omitempty"` // extract backend (success only)
	FinalURL string `json:"finalUrl,omitempty"` // resolved URL after redirects (success only)
}
