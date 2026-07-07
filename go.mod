module github.com/jeffbstewart/cloister

go 1.25.0

// Dependency policy: stdlib + gopkg.in/yaml.v3 + the official MCP Go SDK
// (github.com/modelcontextprotocol/go-sdk; its schema type
// github.com/google/jsonschema-go is imported directly only to declare tool
// input schemas). Anything beyond that needs a documented justification.
// Requirements are added by `go mod tidy` as packages migrate in.

require (
	github.com/google/jsonschema-go v0.4.3
	github.com/modelcontextprotocol/go-sdk v1.6.1
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sys v0.41.0 // indirect
)
