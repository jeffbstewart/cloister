package audit

// Builder-action decisions.
const (
	DecisionRun                Decision = "run"
	DecisionRejectedParam      Decision = "rejected_param"
	DecisionRejectedBusy       Decision = "rejected_busy"
	DecisionRejectedNoManifest Decision = "rejected_no_manifest"
)

// CommandDetail is a builder command's record body: a manifest action
// invocation — build, test, coverage, lint, etc.
type CommandDetail struct {
	Params   map[string]string `json:"params,omitempty"` // the agent-suppliable inputs
	Argv     []string          `json:"argv,omitempty"`   // the fully resolved command run
	ExitCode *int              `json:"exitCode,omitempty"`
	LogPath  string            `json:"logPath,omitempty"`
	LogBytes int64             `json:"logBytes,omitempty"`
}
