// Copyright 2026 Jeffrey B. Stewart
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package composelint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests MUTATE THE COMMITTED FILE rather than building a fixture from
// scratch, and that choice is the point.  A hand-written fixture drifts: it
// keeps passing while the real file grows a service the fixture never had,
// and the suite reports a rule as covered when it is covered only against a
// document nobody deploys.  Starting from the deployed topology and breaking
// exactly one thing proves the rule fires on the file that matters.
//
// It also removes a whole class of false pass.  A fixture that fails to
// express the violation and a checker that fails to detect it produce the
// same green test; mutating a known-clean document cannot, because the
// helper asserts the mutation actually applied before drawing any conclusion
// from what the checker said.

func readDoc(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", "docker", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// mutation is one broken-topology case: replace `old` with `new` in the
// committed file, and expect a violation mentioning `want`.
type mutation struct {
	name string
	old  string
	new  string
	want string
}

func runMutations(t *testing.T, doc string, check func([]byte) ([]string, error), cases []mutation) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broken := strings.Replace(doc, tc.old, tc.new, 1)
			// Load-bearing: a replacement that matched nothing would leave a
			// CLEAN document, the checker would rightly say nothing, and the
			// only thing proved would be that the anchor string has drifted.
			if broken == doc {
				t.Fatalf("mutation anchor not found in the committed file — update the test:\n%q", tc.old)
			}
			v, err := check([]byte(broken))
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			if !strings.Contains(strings.Join(v, "\n"), tc.want) {
				t.Errorf("no violation mentioning %q; got:\n  - %s", tc.want,
					strings.Join(v, "\n  - "))
			}
		})
	}
}

// ── the abbey overlay ──────────────────────────────────────────────────────

// TestCommittedClaudeDoorIsContained runs the lint against the real file, so
// a commit that widens the door fails the suite.
func TestCommittedClaudeDoorIsContained(t *testing.T) {
	v, err := CheckInfraClaude([]byte(readDoc(t, "abbey-claude.yaml")))
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 0 {
		t.Errorf("committed abbey-claude.yaml violates containment:\n  - %s",
			strings.Join(v, "\n  - "))
	}
}

func TestCatchesClaudeDoorViolations(t *testing.T) {
	doc := readDoc(t, "abbey-claude.yaml")
	runMutations(t, doc, CheckInfraClaude, []mutation{
		// The two that matter most, first.  Either one turns the door into
		// something much wider than the topology diagram suggests, and
		// neither would produce a runtime symptom to notice.
		{
			name: "the request gate is not loaded",
			old:  `- "/etc/cloister/claude-door/cloister_door.py"`,
			new:  `- "/etc/cloister/claude-door/passthrough.py"`,
			want: "bare MITM of one hostname",
		},
		{
			name: "the agent can dial the injecting hop directly",
			old:  "      - claudeplain                      # reachable by claude-proxy ONLY",
			new:  "      - claudeplain\n      - claudenet",
			want: "around the path allowlist",
		},
		{
			name: "the upstream is TLS, collapsing the plaintext segment",
			old:  `- "reverse:http://claude-egress:8080"`,
			new:  `- "reverse:https://api.anthropic.com"`,
			want: "plaintext segment the capture depends on",
		},
		{
			name: "the proxy no longer answers to api.anthropic.com",
			old:  "        aliases: [api.anthropic.com]",
			new:  "        aliases: [anthropic-door]",
			want: "must carry the `api.anthropic.com` alias",
		},
		{
			name: "the stand-down defaults open",
			old:  "CLOISTER_DOOR_ALLOW_SERVER_TOOLS=${CLAUDE_DOOR_ALLOW_SERVER_TOOLS:-0}",
			new:  "CLOISTER_DOOR_ALLOW_SERVER_TOOLS=${CLAUDE_DOOR_ALLOW_SERVER_TOOLS:-1}",
			want: "must default to 0",
		},
		{
			name: "a second container holds the credential",
			old:  "      - ${CLAUDE_LEAF_CERT:?",
			new:  "      - ${ANTHROPIC_TOKEN_FILE}:/run/secrets/anthropic-token:ro\n      - ${CLAUDE_LEAF_CERT:?",
			want: "claude-proxy mounts the Anthropic credential",
		},
		{
			name: "the credential is mounted writable",
			old:  "}:/run/secrets/anthropic-token:ro",
			new:  "}:/run/secrets/anthropic-token",
			want: "credential mount is not `:ro`",
		},
		{
			name: "the proxy gains its own route to the internet",
			old:  "      claudeplain: {}                    # to the last hop; no internet here",
			new:  "      claudeplain: {}\n      egress: {}",
			want: "exactly ONE internet holder",
		},
		{
			name: "the tap runs continuously",
			old:  `    profiles: ["capture"]`,
			new:  "    profiles: []",
			want: "behind the `capture` profile",
		},
		{
			name: "the tap is merely attached to the captured network",
			old:  `    network_mode: "service:claude-proxy"`,
			new:  "    networks: [claudeplain]",
			want: "docker networks are switched",
		},
		{
			name: "the tap takes a capability beyond the raw socket",
			old:  "    cap_add: [NET_RAW]",
			new:  "    cap_add: [NET_RAW, NET_ADMIN]",
			want: "exactly NET_RAW",
		},
		{
			name: "the cell-facing network loses its stable name",
			old:  "    name: claudenet                      # stable: cells join it by name",
			new:  "    # no stable name; cells join it by name",
			want: "stable `name: claudenet`",
		},
		{
			name: "the cell-facing network gains a route out",
			old:  "  claudenet:\n    internal: true",
			new:  "  claudenet:\n    internal: false",
			want: "`claudenet` is not `internal: true`",
		},
		{
			name: "the plaintext segment gains a route out",
			old:  "  claudeplain:\n    internal: true",
			new:  "  claudeplain:\n    internal: false",
			want: "`claudeplain` is not `internal: true`",
		},
		{
			name: "a fourth service joins the door",
			old:  "networks:\n  claudenet:",
			new:  "  claude-extra:\n    image: busybox\nnetworks:\n  claudenet:",
			want: "the claude door is exactly",
		},
		{
			name: "the door holds a workspace",
			old:  "      - claude_captures:/captures",
			new:  "      - grange:/grange",
			want: "the door carries bytes, it does not hold them",
		},
		{
			name: "the door takes the docker control plane",
			old:  "      - claude_captures:/captures",
			new:  "      - /var/run/docker.sock:/var/run/docker.sock",
			want: "root-equivalent control of the host",
		},
		{
			name: "the proxy's dead-DNS pin is dropped",
			old:  "    dns: 127.0.0.1                       # dead upstream",
			new:  "    # dead upstream",
			want: "pin `dns: 127.0.0.1`",
		},
	})
}

// ── the cell overlay ───────────────────────────────────────────────────────

func TestCommittedClaudeCellEdgeIsContained(t *testing.T) {
	v, err := CheckCellClaude([]byte(readDoc(t, "cell-claude.yaml")))
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 0 {
		t.Errorf("committed cell-claude.yaml violates containment:\n  - %s",
			strings.Join(v, "\n  - "))
	}
}

func TestCatchesClaudeCellViolations(t *testing.T) {
	doc := readDoc(t, "cell-claude.yaml")
	runMutations(t, doc, CheckCellClaude, []mutation{
		// The placement-B guarantee, three ways.  Each of these puts a real
		// credential in a container the model controls, which quietly turns
		// every packet capture into a credential disclosure — and nothing at
		// runtime would say a word.
		{
			name: "a real token replaces the placeholder",
			old:  "ANTHROPIC_AUTH_TOKEN=placeholder-the-door-replaces-this",
			new:  "ANTHROPIC_AUTH_TOKEN=a-real-credential-would-go-here",
			want: "must be a literal placeholder",
		},
		{
			name: "the placeholder becomes a deploy-time slot",
			old:  "ANTHROPIC_AUTH_TOKEN=placeholder-the-door-replaces-this",
			new:  "ANTHROPIC_AUTH_TOKEN=${ANTHROPIC_PLACEHOLDER}",
			want: "must not be deploy-substituted",
		},
		{
			name: "a second credential header sneaks in",
			old:  "      - DISABLE_AUTOUPDATER=1",
			new:  "      - CLAUDE_CODE_OAUTH_TOKEN=${REAL_TOKEN}\n      - DISABLE_AUTOUPDATER=1",
			want: "agent sets CLAUDE_CODE_OAUTH_TOKEN",
		},
		{
			name: "an API key sneaks in",
			old:  "      - DISABLE_AUTOUPDATER=1",
			new:  "      - ANTHROPIC_API_KEY=${REAL_KEY}\n      - DISABLE_AUTOUPDATER=1",
			want: "agent sets ANTHROPIC_API_KEY",
		},
		// Fidelity: a base-URL redirect works for the calls that honour it
		// and silently breaks the ones that do not, which are exactly the
		// calls this diagnostic exists to observe.
		{
			name: "the harness is redirected by base URL instead of by alias",
			old:  "      - DISABLE_AUTOUPDATER=1",
			new:  "      - ANTHROPIC_BASE_URL=http://claude-proxy:443\n      - DISABLE_AUTOUPDATER=1",
			want: "agent sets ANTHROPIC_BASE_URL",
		},
		{
			name: "the trust store falls back to the bundled roots",
			old:  "      - CLAUDE_CODE_CERT_STORE=system",
			new:  "      - CLAUDE_CODE_CERT_STORE=bundled",
			want: "must be `system`",
		},
		{
			name: "TLS validation is switched off wholesale",
			old:  "      - DISABLE_AUTOUPDATER=1",
			new:  "      - NODE_TLS_REJECT_UNAUTHORIZED=0\n      - DISABLE_AUTOUPDATER=1",
			want: "disables validation globally",
		},
		{
			name: "the overlay grants a second edge",
			old:  "      - claudenet\n",
			new:  "      - claudenet\n      - egress\n",
			want: "adds exactly ONE edge",
		},
		{
			name: "the door network is declared locally instead of joined",
			old:  "    external: true                       # the abbey's claude door",
			new:  "    internal: true                       # the abbey's claude door",
			want: "must be `external: true`",
		},
		{
			name: "the overlay reaches past the agent",
			old:  "networks:\n  claudenet:",
			new:  "  archivist:\n    dns: 127.0.0.1\nnetworks:\n  claudenet:",
			want: "may touch the `agent` alone",
		},
		{
			name: "the overlay adds a mount the cell's lint never saw",
			old:  "    dns: 127.0.0.1",
			new:  "    volumes:\n      - /srv/secrets:/secrets:ro\n    dns: 127.0.0.1",
			want: "the cell's mounts are cell.yaml's",
		},
		{
			name: "the dead-DNS pin is dropped at the site that adds a network",
			old:  "    dns: 127.0.0.1",
			new:  "    # dns pin dropped",
			want: "pin `dns: 127.0.0.1`",
		},
		// Session state.  Both of these put transcripts, projects and
		// .claude.json back on the per-project home volume, where they
		// survive `dispose` — a persistence channel outliving the very
		// workspace destruction this design turns on.
		{
			name: "the harness state directory is not relocated",
			old:  "      - CLAUDE_CONFIG_DIR=/home/agent/.claude-session",
			new:  "      # state dir left at the default",
			want: "must set CLAUDE_CONFIG_DIR",
		},
		{
			name: "the state directory is relocated but not made ephemeral",
			old:  "      - /home/agent/.claude-session:size=256m,uid=1000,gid=1000,mode=0700",
			new:  "      - /home/agent/somewhere-else:size=256m",
			want: "no tmpfs is mounted there",
		},
	})
}

func TestIdentifiesTheClaudeOverlays(t *testing.T) {
	for _, tc := range []struct {
		name string
		file string
		want Stack
	}{
		{"the abbey overlay", "abbey-claude.yaml", StackInfraClaude},
		{"the cell overlay", "cell-claude.yaml", StackCellClaude},
		// Precedence, not preference: the base sentinels win.  A claude-door
		// service smuggled into the abbey must identify as the ABBEY, so the
		// abbey's own exactly-three-egress rule is what refuses it — rather
		// than the file quietly earning the overlay's weaker membership
		// rules by carrying the right service name.
		{"the base abbey still wins", "abbey.yaml", StackInfra},
		{"the base cell still wins", "cell.yaml", StackCell},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Identify([]byte(readDoc(t, tc.file)))
			if err != nil {
				t.Fatalf("Identify: %v", err)
			}
			if got != tc.want {
				t.Errorf("Identify(%s) = %q, want %q", tc.file, got, tc.want)
			}
		})
	}
}

// TestClaudeDoorAddonEnforcementSurface guards the request gate's content,
// not just that compose loads it.  The two live here together on purpose:
// compose-lint asserts claude-proxy loads cloister_door.py, and without this
// that assertion is satisfied by an addon which refuses nothing.
//
// It matters more than a text-match test usually would.  This addon is the
// ONLY enforcement standing behind the grounding invariant — `permissions.deny`
// on WebSearch was measured inert under bypassPermissions, which is exactly
// the mode M3 runs in — so a quiet edit here has no other backstop, and no
// runtime symptom either: the door would keep working, just wider.
func TestClaudeDoorAddonEnforcementSurface(t *testing.T) {
	path := filepath.Join("..", "..", "docker", "claude-door", "proxy", "cloister_door.py")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(data)

	for _, want := range []struct{ token, why string }{
		{`"/v1/messages"`, "the path allowlist — api.anthropic.com is also a STORAGE service (Files, Batches), so a host pin without a path pin lets the grange be uploaded and collected from outside"},
		{`"web_search"`, "server-side search runs past our last hop, where no capture or relay can follow"},
		{`"web_fetch"`, "same hole as web_search, different name"},
		{`"code_execution"`, "a server-side container is an egress path with no capture"},
		{`"mcp_servers"`, "the server-side MCP connector is the same hole in a third shape"},
		{`flow.client_conn.sni`, "the host check must read the SNI the agent presented, not a Host header any hop could rewrite"},
	} {
		if !strings.Contains(src, want.token) {
			t.Errorf("cloister_door.py no longer mentions %s\n  why it was there: %s", want.token, want.why)
		}
	}

	// Refusal must be the DEFAULT.  The stand-down exists for on-host
	// evaluation, where no scholar is reachable to redirect research to; a
	// default-open build would stand the control down everywhere and say
	// nothing about it.
	if !strings.Contains(src, `os.environ.get("CLOISTER_DOOR_ALLOW_SERVER_TOOLS", "") == "1"`) {
		t.Error("the server-side-tool stand-down is no longer an exact opt-in to \"1\" — anything looser (a truthiness test, a default) turns an operator-only escape hatch into the normal case")
	}
}

func TestDefaultsToZero(t *testing.T) {
	for _, tc := range []struct {
		val  string
		want bool
	}{
		{"0", true},
		{" 0 ", true},
		{"${FOO:-0}", true},
		{"${FOO-0}", true},
		{"1", false},
		{"${FOO:-1}", false},
		{"${FOO}", false},
		{"", false},
	} {
		if got := defaultsToZero(tc.val); got != tc.want {
			t.Errorf("defaultsToZero(%q) = %v, want %v", tc.val, got, tc.want)
		}
	}
}
