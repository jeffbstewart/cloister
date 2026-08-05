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
	"fmt"
	"slices"
	"sort"
	"strings"
)

// The jailed-claude door (docs/JAILED_CLAUDE.md) is a deliberate, scoped
// relaxation of the cell's network containment, and it lands as two OVERLAY
// files rather than as edits to docker/abbey.yaml and docker/cell.yaml.
// That shape is what these checks exist to protect: the base stacks keep
// their invariants verbatim — exactly three containers holding `egress`,
// the agency as the sole inference door, the agent on exactly three
// networks — and merging an overlay is the act that trades them away.
//
// So the overlays get their OWN invariant set, and it is not a weaker one.
// What the base rules assert absolutely, these assert about the delta:
// exactly one new egress holder, exactly one new agent edge, and the
// credential in exactly one container.
//
// Four properties are load-bearing enough to name here, because each is a
// case where the topology is doing work no configuration flag could:
//
//   1. claude-egress must NOT hold claudenet.  It is the only container
//      holding the Anthropic credential, and it injects that credential
//      into whatever reaches it.  An agent that could dial it directly
//      would have an authenticated path to the whole API around the path
//      allowlist — which is the one control standing between "a pinned
//      host" and "a storage service that will hold your source for you."
//   2. claude-proxy must load the addon.  Without it this is a bare MITM
//      of one hostname: no path allowlist, no server-side-tool refusal,
//      and no observation log.  The `-s` argument is the difference
//      between a door and a hole, the same way the agency's absent
//      `-upstream` is the difference between routing and pass-through.
//   3. The cell's token must be a PLACEHOLDER.  Placement B is the whole
//      reason a packet capture of the plaintext hop is safe to take: the
//      real credential is injected downstream of every capture point.  A
//      real token in the cell silently converts every pcap into a
//      credential disclosure, and nothing at runtime would complain.
//   4. No ANTHROPIC_BASE_URL.  The agent is meant to dial its real
//      endpoint and reach the door by network alias, so the calls that
//      IGNORE a base URL arrive at the door too instead of silently
//      failing — that is the fidelity the diagnostic depends on.

// claudeDoorServices are the three containers the abbey overlay may define,
// and the only three.  A fourth service in this file would be a shared door
// nobody reviewed.
var claudeDoorServices = []string{"claude-egress", "claude-proxy", "claude-tap"}

// anthropicTokenPath is where the credential lands in claude-egress.  It is
// named once here because two separate checks depend on it: that
// claude-egress mounts it read-only, and that nothing else mounts it at all.
const anthropicTokenPath = "/run/secrets/anthropic-token"

// CheckInfraClaude returns the violations of the jailed-claude door's abbey
// overlay (docker/abbey-claude.yaml); an empty slice means the file is
// clean.
func CheckInfraClaude(data []byte) ([]string, error) {
	c, err := parse(data)
	if err != nil {
		return nil, err
	}
	var v []string

	if got := serviceNames(c); !slices.Equal(got, claudeDoorServices) {
		v = append(v, fmt.Sprintf("the claude door is exactly %v; this file defines %v", claudeDoorServices, got))
	}

	// The networks.  claudenet is the cell-facing edge and needs a STABLE
	// name, because a cell joins it by that name from a different compose
	// project — unset, compose would prefix it and the two would silently
	// be different wires.
	if def, ok := c.Networks["claudenet"]; !ok {
		v = append(v, "no `claudenet` network defined — cells have no edge to the door")
	} else {
		if !def.Internal {
			v = append(v, "`claudenet` is not `internal: true` — the agent's edge must grant no route out by itself; the proxy is the only thing on it")
		}
		if def.Name != "claudenet" {
			v = append(v, fmt.Sprintf("`claudenet` must carry the stable `name: claudenet` — cells join it by name from another compose project; name = %q", def.Name))
		}
	}
	if def, ok := c.Networks["claudeplain"]; !ok {
		v = append(v, "no `claudeplain` network defined — the door has no plaintext segment, which is the whole capture point")
	} else if !def.Internal {
		v = append(v, "`claudeplain` is not `internal: true` — the cleartext hop must be reachable by nothing but its two ends")
	}

	// Membership, both ways.  These are the checks that keep the door a
	// chain rather than a mesh: each network has exactly its two ends.
	if h := holdersOf(c, "claudenet"); !slices.Equal(h, []string{"claude-proxy"}) {
		v = append(v, fmt.Sprintf("claudenet membership in the abbey must be exactly claude-proxy (cells join it from their own stack); got %v", h))
	}
	if h := holdersOf(c, "claudeplain"); !slices.Equal(h, []string{"claude-egress", "claude-proxy"}) {
		v = append(v, fmt.Sprintf("claudeplain membership must be exactly claude-proxy and claude-egress — the captured segment has two ends; got %v", h))
	}
	// The abbey's fourth internet holder, and it must be the only one this
	// overlay adds.  docker/abbey.yaml's own rule still pins the base three.
	if h := holdersOf(c, "egress"); !slices.Equal(h, []string{"claude-egress"}) {
		v = append(v, fmt.Sprintf("this overlay may add exactly ONE internet holder (claude-egress); got %v", h))
	}

	v = append(v, claudeProxyViolations(c)...)
	v = append(v, claudeEgressViolations(c)...)
	v = append(v, claudeTapViolations(c)...)

	// The credential lives in exactly one container.  Same property as the
	// archivist's bot token: the secret never enters a container the model
	// can reach, and here that is enforced by counting mounts rather than
	// by trusting the reader of a compose file to notice a second one.
	for _, name := range serviceNames(c) {
		if name == "claude-egress" {
			continue
		}
		if mounted, _ := c.Services[name].mountsInto(anthropicTokenPath); mounted {
			v = append(v, fmt.Sprintf("%s mounts the Anthropic credential — claude-egress is its only holder, which is what makes a capture of the plaintext hop safe to take", name))
		}
	}
	// No workspace, ever.  The door handles the agent's traffic; it has no
	// business holding the agent's source.
	for _, name := range serviceNames(c) {
		for _, vol := range c.Services[name].Volumes {
			if strings.Contains(vol, ":/grange") || strings.Contains(vol, ":/workspace") {
				v = append(v, fmt.Sprintf("%s mounts a workspace (%q) — the door carries bytes, it does not hold them", name, vol))
			}
		}
	}

	v = append(v, dockerSocketViolations(c)...)
	v = append(v, dnsPinViolations(c)...)
	return v, nil
}

// claudeProxyViolations checks the inspecting hop: the agent's whole view of
// Anthropic, and the container where enforcement actually lives.
func claudeProxyViolations(c compose) []string {
	p, ok := c.Services["claude-proxy"]
	if !ok {
		return []string{"no `claude-proxy` service defined — the door has no inspecting hop"}
	}
	var v []string

	nets := p.Networks.names()
	sort.Strings(nets)
	if !slices.Equal(nets, []string{"claudenet", "claudeplain"}) {
		v = append(v, fmt.Sprintf("claude-proxy networks must be exactly claudenet+claudeplain; got %v", nets))
	}
	for _, n := range p.egressCapable("claudenet", "claudeplain") {
		v = append(v, fmt.Sprintf("claude-proxy holds %q — the inspecting hop reaches the internet only through claude-egress", n))
	}
	// The alias is why no ANTHROPIC_BASE_URL is needed, and why the calls
	// that ignore a base URL still land here.
	if !slices.Contains(p.Networks.aliasesOn("claudenet"), "api.anthropic.com") {
		v = append(v, "claude-proxy must carry the `api.anthropic.com` alias on claudenet — without it the agent needs ANTHROPIC_BASE_URL, and every hard-coded Anthropic call silently fails instead of being captured")
	}
	// The request gate.  See property 2 in the file comment.
	if !slices.ContainsFunc(p.Command, func(a string) bool {
		return strings.HasSuffix(a, "cloister_door.py")
	}) {
		v = append(v, fmt.Sprintf("claude-proxy must load the cloister_door.py addon (`-s <path>`) — without it this is a bare MITM of one hostname, with no path allowlist and no server-side-tool refusal; command = %v", p.Command))
	}
	if !slices.Contains(p.Command, "reverse:http://claude-egress:8080") {
		v = append(v, fmt.Sprintf("claude-proxy must forward to the literal `reverse:http://claude-egress:8080` — an https upstream would collapse the plaintext segment the capture depends on; command = %v", p.Command))
	}
	// Default-closed, checked statically.  The stand-down exists for on-host
	// evaluation, where no scholar is reachable; a cell that inherited it
	// switched off a control without saying so.
	if val, set := p.env("CLOISTER_DOOR_ALLOW_SERVER_TOOLS"); set && !defaultsToZero(val) {
		v = append(v, fmt.Sprintf("CLOISTER_DOOR_ALLOW_SERVER_TOOLS must default to 0 — the server-side-tool refusal is stood down only by an explicit operator opt-in; value = %q", val))
	}
	if p.runsAsRoot() {
		v = append(v, fmt.Sprintf("claude-proxy must run as a non-root user; user = %q", p.User))
	}
	return v
}

// claudeEgressViolations checks the injecting hop — the container that holds
// the credential, and the abbey's fourth route to the internet.
func claudeEgressViolations(c compose) []string {
	e, ok := c.Services["claude-egress"]
	if !ok {
		return []string{"no `claude-egress` service defined — the door has no way out and no credential"}
	}
	var v []string

	nets := e.Networks.names()
	sort.Strings(nets)
	if !slices.Equal(nets, []string{"claudeplain", "egress"}) {
		v = append(v, fmt.Sprintf("claude-egress networks must be exactly claudeplain+egress; got %v", nets))
	}
	// Called out separately from the membership check above, because this is
	// the specific failure worth naming: it is not a stray network, it is a
	// path from the model to an authenticated API around the allowlist.
	if e.hasNet("claudenet") {
		v = append(v, "claude-egress holds `claudenet` — the agent must never reach the injecting hop directly; that is an authenticated path to the whole Anthropic API around the path allowlist")
	}
	mounted, readOnly := e.mountsInto(anthropicTokenPath)
	switch {
	case !mounted:
		v = append(v, fmt.Sprintf("claude-egress has no credential mounted at %s — the hop that exists to inject it has nothing to inject", anthropicTokenPath))
	case !readOnly:
		v = append(v, "claude-egress's credential mount is not `:ro` — the door reads the secret, it never rewrites it")
	}
	return v
}

// claudeTapViolations checks the capture sidecar: a short-lived diagnostic,
// off unless asked for, inside the proxy's namespace, writing nowhere but
// the captures volume.
func claudeTapViolations(c compose) []string {
	t, ok := c.Services["claude-tap"]
	if !ok {
		return []string{"no `claude-tap` service defined — the plaintext hop has no capture point"}
	}
	var v []string

	// Namespace-sharing, not attachment.  Docker networks are switched
	// bridges: a sniffer merely ON claudeplain sees none of the unicast
	// traffic between its two ends.
	if t.NetworkMode != "service:claude-proxy" {
		v = append(v, fmt.Sprintf("claude-tap must run inside claude-proxy's network namespace (`network_mode: \"service:claude-proxy\"`) — a container merely attached to claudeplain captures nothing, because docker networks are switched; network_mode = %q", t.NetworkMode))
	}
	if len(t.Networks) != 0 {
		v = append(v, fmt.Sprintf("claude-tap must declare no networks of its own — it has claude-proxy's; got %v", t.Networks.names()))
	}
	// Profile-gated.  A capture is something the operator switches on to
	// answer one question and off when it is answered — never a durable
	// record, which is the state service's job.  Running by default is how
	// a diagnostic quietly becomes an audit log nobody decided to keep.
	if !slices.Contains(t.Profiles, "capture") {
		v = append(v, fmt.Sprintf("claude-tap must sit behind the `capture` profile — a decrypted capture is the most sensitive artifact this design produces, and its risk grows with its lifetime; profiles = %v", t.Profiles))
	}
	if !slices.Equal(t.CapAdd, []string{"NET_RAW"}) {
		v = append(v, fmt.Sprintf("claude-tap may add exactly NET_RAW (tcpdump's raw socket) and nothing else; cap_add = %v", t.CapAdd))
	}
	if mounted, _ := t.mountsInto("/captures"); !mounted {
		v = append(v, "claude-tap has no `/captures` mount — the capture has nowhere to land that is not a container layer")
	}
	for _, vol := range t.Volumes {
		if !strings.Contains(vol, ":/captures") {
			v = append(v, fmt.Sprintf("claude-tap mounts %q — the tap writes captures and reads nothing", vol))
		}
	}
	return v
}

// CheckCellClaude returns the violations of the cell's claude-door overlay
// (docker/cell-claude.yaml); an empty slice means the file is clean.  The
// overlay is one network edge and the harness's environment — anything more
// is a change to the cell that the cell's own lint never saw.
func CheckCellClaude(data []byte) ([]string, error) {
	c, err := parse(data)
	if err != nil {
		return nil, err
	}
	var v []string

	if got := serviceNames(c); !slices.Equal(got, []string{"agent", "archivist"}) {
		v = append(v, fmt.Sprintf("this overlay may touch the `agent` and the `archivist` alone — the workspace and the PR gate are what the experiment TESTS, not what it changes; services = %v", got))
	}
	v = append(v, claudeDisclosureViolations(c)...)
	if def, ok := c.Networks["claudenet"]; !ok {
		v = append(v, "network `claudenet` is not declared — a cell joins the abbey's door by name")
	} else {
		if !def.External {
			v = append(v, "`claudenet` must be `external: true` — it is the abbey's door; declared locally the cell gets a private copy of it and reaches nothing")
		}
		if def.Name != "claudenet" {
			v = append(v, fmt.Sprintf("`claudenet` must name the abbey's network (`name: claudenet`); name = %q", def.Name))
		}
	}

	agent, ok := c.Services["agent"]
	if !ok {
		return v, nil
	}
	if nets := agent.Networks.names(); !slices.Equal(nets, []string{"claudenet"}) {
		v = append(v, fmt.Sprintf("the overlay adds exactly ONE edge, claudenet — the agent's other three are cell.yaml's and are linted there; got %v", nets))
	}
	for _, n := range agent.egressCapable("claudenet") {
		v = append(v, fmt.Sprintf("agent holds %q — the claude door is the only route out this overlay grants", n))
	}
	// No new mounts.  The grange is the only workspace and the home volume
	// is cell.yaml's; a volume added here is one the cell's lint never saw.
	if len(agent.Volumes) != 0 {
		v = append(v, fmt.Sprintf("the overlay declares volumes (%v) — the cell's mounts are cell.yaml's, where the one-workspace rule is enforced", agent.Volumes))
	}

	v = append(v, claudeCellEnvViolations(agent)...)
	v = append(v, claudeSessionStateViolations(agent)...)
	v = append(v, dnsPinViolations(c)...)
	return v, nil
}

// claudeDisclosureViolations enforces the pairing that keeps the disclosure
// gate from being fail-open in practice.
//
// The gate is armed by an environment variable on the archivist, and an
// archivist whose cell sends source to Anthropic without it would simply
// never ask.  Nothing at runtime could notice: the archivist has no route
// to the claude door and cannot tell whether one exists.  So the check
// lives here, on the file that grants the agent its edge — you cannot merge
// the overlay that sends source to Anthropic without also merging the line
// that demands it be acknowledged.
//
// The archivist stanza is held to exactly that, and nothing else.  This
// overlay has no business touching the component that owns version control,
// the bot credential, and the PR gate — those are the guarantees the whole
// experiment exists to TEST, not to modify.
func claudeDisclosureViolations(c compose) []string {
	arc, ok := c.Services["archivist"]
	if !ok {
		return []string{"the overlay does not arm the disclosure gate — an `archivist` stanza setting CLOISTER_DISCLOSURE_REQUIRED is what makes provision ask, per repository, before this cell's source goes to Anthropic (docs/JAILED_CLAUDE.md)"}
	}
	var v []string
	if to, set := arc.env("CLOISTER_DISCLOSURE_REQUIRED"); !set || strings.TrimSpace(to) == "" {
		v = append(v, "archivist must set CLOISTER_DISCLOSURE_REQUIRED to where source would go (e.g. `anthropic`) — unset, the gate is inert and provision never asks")
	}
	// Everything else about the archivist is off-limits here.  A network, a
	// volume, or an image swapped in this file would change the jailed
	// owner of version control in a document about a proxy.
	if len(arc.Networks) != 0 {
		v = append(v, fmt.Sprintf("the overlay gives the archivist networks (%v) — its three edges are cell.yaml's, where they are linted; this file grants the AGENT one edge and nothing else", arc.Networks.names()))
	}
	if len(arc.Volumes) != 0 {
		v = append(v, fmt.Sprintf("the overlay gives the archivist volumes (%v) — the grange and the bot credential are cell.yaml's business", arc.Volumes))
	}
	if arc.Image != "" || len(arc.Entrypoint) != 0 || len(arc.Command) != 0 {
		v = append(v, "the overlay changes what the archivist RUNS — it may arm the disclosure gate and nothing more")
	}
	// The acknowledgment itself must not be committed.  Its whole purpose is
	// that an operator typed it for THIS repository; a value in the tree is
	// one inherited by every cell that ever deploys this file, which is the
	// boolean failure mode wearing a different name.
	for _, e := range arc.Environment {
		name, _, _ := strings.Cut(e, "=")
		if strings.HasPrefix(name, "CLOISTER_DISCLOSURE_") && name != "CLOISTER_DISCLOSURE_REQUIRED" {
			v = append(v, fmt.Sprintf("%s is committed to the tree — the acknowledgment is the operator's, set per repository on the stack; a committed one is inherited by every cell that deploys this file, which is exactly the copy-paste failure the per-repository naming exists to catch", name))
		}
	}
	return v
}

// claudeSessionStateViolations keeps the harness's state off the per-project
// home volume.  `~/.claude` there would survive `dispose`, turning
// transcripts and auto-memory into a persistence channel that outlives the
// workspace destruction this whole design turns on — which is the one risk
// in JAILED_CLAUDE.md with no invariant of its own to name.
//
// It is checked HERE, in topology, rather than trusted to the
// `autoMemoryDirectory` setting, because this CLI silently ignores settings
// keys it does not recognize: a key that does nothing and a key that works
// are indistinguishable from the outside.  A tmpfs is neither.
func claudeSessionStateViolations(agent service) []string {
	dir, set := agent.env("CLAUDE_CONFIG_DIR")
	if !set {
		return []string{"agent must set CLAUDE_CONFIG_DIR — left unset, Claude Code keeps sessions, projects and .claude.json under $HOME, which is a per-project volume that survives `dispose`"}
	}
	if !agent.hasTmpfsAt(dir) {
		return []string{fmt.Sprintf("CLAUDE_CONFIG_DIR is %q but no tmpfs is mounted there — without one the harness's whole state tree lands on the home volume and outlives the workspace; a settings key is not a substitute, since unrecognized keys are silently ignored", dir)}
	}
	return nil
}

// claudeCellEnvViolations checks the harness environment.  Three of these
// are containment rather than configuration: the placeholder credential is
// what makes every capture credential-free, the absent base URL is what
// keeps the diagnostic faithful, and the trust store is the second
// guarantee behind the topology.
func claudeCellEnvViolations(agent service) []string {
	var v []string

	tok, set := agent.env("ANTHROPIC_AUTH_TOKEN")
	switch {
	case !set:
		v = append(v, "agent must set ANTHROPIC_AUTH_TOKEN — unset, the harness tries to open a browser login flow that cannot possibly complete inside a jail")
	case !strings.Contains(strings.ToLower(tok), "placeholder"):
		v = append(v, fmt.Sprintf("ANTHROPIC_AUTH_TOKEN must be a literal placeholder — claude-egress injects the real credential downstream of every capture point, and a real token here turns every pcap into a credential disclosure with nothing at runtime to complain; value = %q", tok))
	case strings.Contains(tok, "${"):
		v = append(v, fmt.Sprintf("ANTHROPIC_AUTH_TOKEN must not be deploy-substituted — the placeholder is a constant, and a ${} here is a slot a real credential fits into; value = %q", tok))
	}
	// The other two credential headers.  Either one in the cell defeats
	// placement B just as completely as a real ANTHROPIC_AUTH_TOKEN.
	for _, key := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"} {
		if _, set := agent.env(key); set {
			v = append(v, fmt.Sprintf("agent sets %s — no real credential enters a container the model controls; claude-egress injects it (docs/JAILED_CLAUDE.md, placement B)", key))
		}
	}
	if _, set := agent.env("ANTHROPIC_BASE_URL"); set {
		v = append(v, "agent sets ANTHROPIC_BASE_URL — the door is reached by network alias so the harness dials its REAL endpoint; a base-URL redirect means the calls that ignore it (the fast-mode check, the WebFetch preflight) silently fail instead of being observed")
	}
	if store, _ := agent.env("CLAUDE_CODE_CERT_STORE"); store != "system" {
		v = append(v, fmt.Sprintf("CLAUDE_CODE_CERT_STORE must be `system` — the image's trust store holds exactly the cloister CA, so anything but our own issuer fails validation loudly; value = %q", store))
	}
	if _, set := agent.env("NODE_TLS_REJECT_UNAUTHORIZED"); set {
		v = append(v, "agent sets NODE_TLS_REJECT_UNAUTHORIZED — it disables validation globally and throws away the single-certificate trust store to save a config line")
	}
	return v
}

// defaultsToZero reports whether a compose value is 0, or interpolates to 0
// when the variable is unset — `${FOO:-0}` and `${FOO-0}` both qualify.  It
// is how a default-closed switch is checked statically.
func defaultsToZero(val string) bool {
	if strings.TrimSpace(val) == "0" {
		return true
	}
	return strings.HasSuffix(val, ":-0}") || strings.HasSuffix(val, "-0}")
}
