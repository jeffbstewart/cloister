module github.com/jeffbstewart/cloister

go 1.25.0

// Dependency policy: stdlib + gopkg.in/yaml.v3 + the official MCP Go SDK
// (github.com/modelcontextprotocol/go-sdk; its schema type
// github.com/google/jsonschema-go is imported directly only to declare tool
// input schemas). Anything beyond that needs a documented justification.
// Requirements are added by `go mod tidy` as packages migrate in.
