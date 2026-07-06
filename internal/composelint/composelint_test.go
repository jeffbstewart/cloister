package composelint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCommittedCellStackIsContained runs the lint against the real repo file,
// so a commit that breaks the scholar's containment fails the test suite.  It
// skips until the deployment migration lands docker/ai-workers.yaml, then
// guards it forever after.
func TestCommittedCellStackIsContained(t *testing.T) {
	path := filepath.Join("..", "..", "docker", "ai-workers.yaml")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skipf("%s not yet migrated (arrives with the deployment PR)", path)
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	v, err := Check(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 0 {
		t.Errorf("committed ai-workers.yaml violates scholar containment:\n  - %s",
			strings.Join(v, "\n  - "))
	}
}

func TestCatchesViolations(t *testing.T) {
	base := func(scholarNets, scholarVols, relayCmd, extra string) string {
		return `
networks:
  researchnet: { internal: true }
  scholarstate: { internal: true }
  kagiegress: { internal: true }
  statenet: { internal: true }
  egress: {}
services:
  scholar:
    networks: ` + scholarNets + `
    volumes: ` + scholarVols + `
  kagi-relay:
    command: ` + relayCmd + `
    networks: [kagiegress, egress]
` + extra
	}
	clean := `[researchnet, scholarstate, kagiegress]`
	noVols := `[]`
	kagiCmd := `["TCP-LISTEN:8443,fork,reuseaddr", "TCP:kagi.com:443"]`

	cases := map[string]string{
		"scholar on egress":        base(`[researchnet, kagiegress, egress]`, noVols, kagiCmd, ""),
		"scholar on statenet":      base(`[researchnet, statenet, kagiegress]`, noVols, kagiCmd, ""),
		"scholar mounts workspace": base(clean, `["/host:/workspace:ro"]`, kagiCmd, ""),
		"relay not pinned to kagi": base(clean, noVols, `["TCP-LISTEN:8443", "TCP:evil.example:443"]`, ""),
		"scholar net not internal": `
networks:
  researchnet: { internal: true }
  scholarstate: { internal: true }
  kagiegress: {}
  egress: {}
services:
  scholar: { networks: [researchnet, scholarstate, kagiegress] }
  kagi-relay: { command: ["TCP:kagi.com:443"], networks: [kagiegress, egress] }`,
		"second egress holder": base(clean, noVols, kagiCmd, `  sneaky:
    networks: [egress]`),
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			v, err := Check([]byte(yaml))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(v) == 0 {
				t.Errorf("expected a containment violation, got none")
			}
		})
	}

	// And the clean shape passes.
	if v, err := Check([]byte(base(clean, noVols, kagiCmd, ""))); err != nil || len(v) != 0 {
		t.Errorf("clean compose flagged: %v (err %v)", v, err)
	}
}
