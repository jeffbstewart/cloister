package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validPolicy = `
search:
  engine: kagi
  dailyCap: 100
  denySearchEnginePages: true
extract:
  dailyCap: 50
  deny:
    - host: pastebin.com
limits:
  maxResponseBytes: 1048576
  timeout: 20s
`

func writePolicy(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadPolicyValid(t *testing.T) {
	p, err := LoadPolicy(writePolicy(t, validPolicy))
	if err != nil {
		t.Fatal(err)
	}
	if p.Search.Engine != EngineKagi || p.Search.DailyCap != 100 || p.Extract.DailyCap != 50 {
		t.Errorf("unexpected: %+v", p)
	}
	if p.Search.DenySearchEnginePages == nil || !*p.Search.DenySearchEnginePages {
		t.Errorf("toggle not parsed")
	}
}

func TestLoadPolicyMissingFileFails(t *testing.T) {
	if _, err := LoadPolicy(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("want error for missing file")
	}
}

func TestLoadPolicyFailClosed(t *testing.T) {
	tests := map[string]string{
		"unknown key":            strings.Replace(validPolicy, "engine: kagi", "engine: kagi\n  bogus: 1", 1),
		"missing engine":         strings.Replace(validPolicy, "engine: kagi\n", "", 1),
		"bad engine":             strings.Replace(validPolicy, "engine: kagi", "engine: google", 1),
		"zero search cap":        strings.Replace(validPolicy, "dailyCap: 100", "dailyCap: 0", 1),
		"missing toggle":         strings.Replace(validPolicy, "  denySearchEnginePages: true\n", "", 1),
		"zero timeout":           strings.Replace(validPolicy, "timeout: 20s", "timeout: 0s", 1),
		"bad timeout string":     strings.Replace(validPolicy, "timeout: 20s", "timeout: 20", 1),
		"bad deny host wildcard": strings.Replace(validPolicy, "host: pastebin.com", `host: "ev*l.com"`, 1),
	}
	for name, body := range tests {
		if _, err := LoadPolicy(writePolicy(t, body)); err == nil {
			t.Errorf("%s: want fail-closed error, got nil", name)
		}
	}
}

func TestLoadPolicyDenyRequiredUnlessToggle(t *testing.T) {
	// Empty deny + toggle off → error.
	body := strings.Replace(validPolicy, "denySearchEnginePages: true", "denySearchEnginePages: false", 1)
	body = strings.Replace(body, "  deny:\n    - host: pastebin.com\n", "  deny: []\n", 1)
	if _, err := LoadPolicy(writePolicy(t, body)); err == nil {
		t.Error("empty deny + toggle off: want error")
	}
	// Empty deny + toggle on → ok (built-ins cover it).
	body = strings.Replace(validPolicy, "  deny:\n    - host: pastebin.com\n", "  deny: []\n", 1)
	if _, err := LoadPolicy(writePolicy(t, body)); err != nil {
		t.Errorf("empty deny + toggle on: unexpected error %v", err)
	}
}
