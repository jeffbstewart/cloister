package manifest

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// refManifest is the reference manifest for a JVM/Gradle project.
const refManifest = `harness: 1
toolchain: jdk25-gradle

actions:
  build:
    description: Compile the project without running tests
    run: ["./gradlew", "--offline", "--no-daemon", "build", "-x", "test"]
    timeout: 15m
    parser: gradle
  test:
    description: Run the JVM test suite
    run: ["./gradlew", "--offline", "--no-daemon", "test"]
    timeout: 30m
    parser: gradle
    params:
      filter:
        description: Test class/method filter
        flag: "--tests"
        pattern: "^[A-Za-z0-9_.*]+$"

caches:
  - volume: gradle
    env: GRADLE_USER_HOME
    path: /gradle-home
    warmup: ["./gradlew", "--refresh-dependencies", "build"]
`

// man wraps an actions body in a minimal valid manifest header.
func man(actions string) string {
	return "harness: 1\ntoolchain: tc\nactions:\n" + actions
}

const minAction = "  build:\n    run: [\"make\"]\n    timeout: 5m\n"

func TestParseReferenceManifest(t *testing.T) {
	m, err := Parse([]byte(refManifest), "jdk25-gradle")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(m.Actions) != 2 {
		t.Fatalf("got %d actions, want 2", len(m.Actions))
	}
	b := m.Actions["build"]
	if got := b.Timeout.Duration(); got != 15*time.Minute {
		t.Errorf("build timeout = %s, want 15m", got)
	}
	if got := b.ParserName(); got != "gradle" {
		t.Errorf("build parser = %q, want gradle", got)
	}
	wantRun := []string{"./gradlew", "--offline", "--no-daemon", "build", "-x", "test"}
	if !reflect.DeepEqual(b.Run, wantRun) {
		t.Errorf("build run = %v, want %v", b.Run, wantRun)
	}
	tst := m.Actions["test"]
	p := tst.Params["filter"]
	if p == nil || p.Flag != "--tests" {
		t.Fatalf("test param filter = %+v, want flag --tests", p)
	}
	if p.re == nil {
		t.Error("param pattern was not compiled during validation")
	}
	if len(m.Caches) != 1 || m.Caches[0].Env != "GRADLE_USER_HOME" || m.Caches[0].Path != "/gradle-home" {
		t.Errorf("caches = %+v", m.Caches)
	}
}

func TestParserDefaultsToGeneric(t *testing.T) {
	m, err := Parse([]byte(man(minAction)), "tc")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := m.Actions["build"].ParserName(); got != "generic" {
		t.Errorf("ParserName() = %q, want generic", got)
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "wrong harness version",
			yaml:    "harness: 2\ntoolchain: tc\nactions:\n" + minAction,
			wantErr: "supports only 1",
		},
		{
			name:    "unknown top-level key",
			yaml:    man(minAction) + "extra: true\n",
			wantErr: "extra",
		},
		{
			name:    "unknown action key",
			yaml:    man("  build:\n    run: [\"make\"]\n    timeout: 5m\n    shell: bash\n"),
			wantErr: "shell",
		},
		{
			name:    "empty manifest",
			yaml:    "",
			wantErr: "empty manifest",
		},
		{
			name:    "no actions",
			yaml:    "harness: 1\ntoolchain: tc\n",
			wantErr: "at least one action",
		},
		{
			name:    "bad action name",
			yaml:    man("  Build:\n    run: [\"make\"]\n    timeout: 5m\n"),
			wantErr: "must match",
		},
		{
			name:    "reserved name get_log",
			yaml:    man("  get_log:\n    run: [\"make\"]\n    timeout: 5m\n"),
			wantErr: "reserved",
		},
		{
			name:    "reserved name harness_info",
			yaml:    man("  harness_info:\n    run: [\"make\"]\n    timeout: 5m\n"),
			wantErr: "reserved",
		},
		{
			name:    "reserved name write_file",
			yaml:    man("  write_file:\n    run: [\"make\"]\n    timeout: 5m\n"),
			wantErr: "reserved",
		},
		{
			name:    "reserved name read_file",
			yaml:    man("  read_file:\n    run: [\"make\"]\n    timeout: 5m\n"),
			wantErr: "reserved",
		},
		{
			name:    "reserved name apply_patch",
			yaml:    man("  apply_patch:\n    run: [\"make\"]\n    timeout: 5m\n"),
			wantErr: "reserved",
		},
		{
			name:    "reserved name web_fetch",
			yaml:    man("  web_fetch:\n    run: [\"make\"]\n    timeout: 5m\n"),
			wantErr: "reserved",
		},
		{
			name:    "reserved name web_search",
			yaml:    man("  web_search:\n    run: [\"make\"]\n    timeout: 5m\n"),
			wantErr: "reserved",
		},
		{
			name:    "reserved name fetch",
			yaml:    man("  fetch:\n    run: [\"make\"]\n    timeout: 5m\n"),
			wantErr: "reserved",
		},
		{
			name:    "reserved name search",
			yaml:    man("  search:\n    run: [\"make\"]\n    timeout: 5m\n"),
			wantErr: "reserved",
		},
		{
			name:    "empty run",
			yaml:    man("  build:\n    run: []\n    timeout: 5m\n"),
			wantErr: "non-empty exec array",
		},
		{
			name:    "missing timeout",
			yaml:    man("  build:\n    run: [\"make\"]\n"),
			wantErr: "timeout required",
		},
		{
			name:    "bad timeout syntax",
			yaml:    man("  build:\n    run: [\"make\"]\n    timeout: 15minutes\n"),
			wantErr: "15minutes",
		},
		{
			name:    "timeout over server cap",
			yaml:    man("  build:\n    run: [\"make\"]\n    timeout: 61m\n"),
			wantErr: "exceeds the server cap",
		},
		{
			name:    "unknown parser",
			yaml:    man("  build:\n    run: [\"make\"]\n    timeout: 5m\n    parser: maven\n"),
			wantErr: "unknown parser",
		},
		{
			name: "param missing flag",
			yaml: man("  build:\n    run: [\"make\"]\n    timeout: 5m\n" +
				"    params:\n      filter:\n        pattern: \"^x$\"\n"),
			wantErr: "flag required",
		},
		{
			name: "param missing pattern",
			yaml: man("  build:\n    run: [\"make\"]\n    timeout: 5m\n" +
				"    params:\n      filter:\n        flag: \"-f\"\n"),
			wantErr: "pattern required",
		},
		{
			name: "param pattern does not compile",
			yaml: man("  build:\n    run: [\"make\"]\n    timeout: 5m\n" +
				"    params:\n      filter:\n        flag: \"-f\"\n        pattern: \"[\"\n"),
			wantErr: "bad pattern",
		},
		{
			name: "bad param name",
			yaml: man("  build:\n    run: [\"make\"]\n    timeout: 5m\n" +
				"    params:\n      Filter:\n        flag: \"-f\"\n        pattern: \"^x$\"\n"),
			wantErr: "must match",
		},
		{
			name:    "bad cache env name",
			yaml:    man(minAction) + "caches:\n  - volume: v\n    env: gradle_home\n    path: /x\n",
			wantErr: "bad env var name",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml), "tc")
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseRejectsToolchainMismatch(t *testing.T) {
	_, err := Parse([]byte(refManifest), "go")
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("want toolchain mismatch error, got %v", err)
	}
}

func TestParseRejectsTooManyActions(t *testing.T) {
	var b strings.Builder
	b.WriteString("harness: 1\ntoolchain: tc\nactions:\n")
	for i := 0; i <= MaxActions; i++ {
		fmt.Fprintf(&b, "  action%d:\n    run: [\"make\"]\n    timeout: 5m\n", i)
	}
	_, err := Parse([]byte(b.String()), "tc")
	if err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("want max-actions error, got %v", err)
	}
}

func TestArgs(t *testing.T) {
	m, err := Parse([]byte(refManifest), "jdk25-gradle")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a := m.Actions["test"]

	got, err := a.Args(map[string]string{"filter": "WidgetMatcherServiceTest"})
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	want := []string{"--tests", "WidgetMatcherServiceTest"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Args = %v, want %v", got, want)
	}

	// Gradle-style wildcard filters must pass: inside the pattern's
	// character class, `.` and `*` are literals, so the reference pattern
	// admits them as data for Gradle's own --tests matching.
	for _, filter := range []string{
		"com.example.sampleapp.service.*",
		"*ServiceTest",
		"WidgetMatcherServiceTest.matchesAllWidgets",
	} {
		got, err := a.Args(map[string]string{"filter": filter})
		if err != nil {
			t.Errorf("Args(filter=%q) rejected: %v", filter, err)
			continue
		}
		want := []string{"--tests", filter}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Args(filter=%q) = %v, want %v", filter, got, want)
		}
	}

	if got, err := a.Args(nil); err != nil || got != nil {
		t.Errorf("Args(nil) = %v, %v; want nil, nil", got, err)
	}
}

func TestArgsRejects(t *testing.T) {
	m, err := Parse([]byte(refManifest), "jdk25-gradle")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a := m.Actions["test"]

	tests := []struct {
		name    string
		params  map[string]string
		wantErr string
	}{
		{"value with space", map[string]string{"filter": "bad value"}, "does not match"},
		{"shell metacharacters", map[string]string{"filter": "Foo;rm -rf /"}, "does not match"},
		{"flag injection", map[string]string{"filter": "--offline=false"}, "does not match"},
		{"unknown param", map[string]string{"nope": "x"}, "unknown param"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := a.Args(tt.params)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Args(%v) error = %v, want containing %q", tt.params, err, tt.wantErr)
			}
		})
	}

	// An action with no declared params accepts no params at all.
	b := m.Actions["build"]
	if _, err := b.Args(map[string]string{"filter": "X"}); err == nil {
		t.Error("build.Args accepted a param it never declared")
	}
}

func TestArgsSortedOrder(t *testing.T) {
	y := man("  test:\n    run: [\"make\"]\n    timeout: 5m\n" +
		"    params:\n" +
		"      zeta:\n        flag: \"-z\"\n        pattern: \".+\"\n" +
		"      alpha:\n        flag: \"-a\"\n        pattern: \".+\"\n")
	m, err := Parse([]byte(y), "tc")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got, err := m.Actions["test"].Args(map[string]string{"zeta": "2", "alpha": "1"})
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	want := []string{"-a", "1", "-z", "2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Args = %v, want %v (deterministic sorted order)", got, want)
	}
}

// TestArgsPassesValueVerbatim documents the contract: a validated value is passed as
// exactly one argv element, never re-split, re-quoted, or "sanitized".
func TestArgsPassesValueVerbatim(t *testing.T) {
	y := man("  test:\n    run: [\"make\"]\n    timeout: 5m\n" +
		"    params:\n      msg:\n        flag: \"-m\"\n        pattern: \".+\"\n")
	m, err := Parse([]byte(y), "tc")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	v := "--evil value; $(rm -rf /)"
	got, err := m.Actions["test"].Args(map[string]string{"msg": v})
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	want := []string{"-m", v}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Args = %v, want %v (verbatim single element)", got, want)
	}
}

// TestPatternIsFullMatch verifies the server anchors patterns itself: a
// manifest pattern without ^$ still cannot partial-match.
func TestPatternIsFullMatch(t *testing.T) {
	y := man("  test:\n    run: [\"make\"]\n    timeout: 5m\n" +
		"    params:\n      filter:\n        flag: \"-f\"\n        pattern: \"abc\"\n")
	m, err := Parse([]byte(y), "tc")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a := m.Actions["test"]
	if _, err := a.Args(map[string]string{"filter": "abc"}); err != nil {
		t.Errorf("exact match rejected: %v", err)
	}
	if _, err := a.Args(map[string]string{"filter": "xabcx"}); err == nil {
		t.Error("partial match accepted; pattern must be anchored to the full value")
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), DefaultPath), "tc")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want fs.ErrNotExist for a missing manifest, got %v", err)
	}
}
