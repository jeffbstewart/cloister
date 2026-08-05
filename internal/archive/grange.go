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

package archive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jeffbstewart/cloister/internal/endpoint"
	"github.com/jeffbstewart/cloister/internal/forge"
)

// Grange owns the workspace's lifecycle (docs/archivist.md, "Grange
// lifecycle"): it provisions a per-task checkout by cloning the canonical
// remote, gates the clone against the repository's forge protections, and
// disposes of it, handing the live *Archive to the working-tree verbs in
// between.  It brings a checkout into being; the Archive drives one that
// already exists.
//
// Layout under Root: tree/ is the exported checkout the verbs operate on;
// staging/ is where a provision clones and validates before the atomic
// rename that promotes it.  Keeping tree a subdirectory (rather than the
// mount root itself) is what lets the promote be a rename — you cannot
// rename onto a mount point.
//
// State is derived from disk, never memory, so a restart is transparent:
// tree/.git plus the provenance marker is PROVISIONED, an empty (or
// absent) tree is EMPTY, and anything else is CORRUPT — a mounted host
// tree, a half-finished promote — where every verb refuses and recovery
// is host-side.
type Grange struct {
	root    string
	tree    string
	staging string
	git     string
	hooks   string // empty hooks dir for the unpinned clone
	now     func() time.Time

	table      *endpoint.Table
	gate       ProvisionGate
	disclosure func(repo string) error
	openForge  func(endpoint.Endpoint) (forge.Client, error)
	cloner     Cloner
	defBranch  string

	// The live workspace, non-nil only while PROVISIONED.  The archivist
	// serializes every verb behind one lock, so these need no lock of
	// their own — the caller holds it around provision, dispose, and every
	// verb that reads arc.
	arc   *Archive
	forge forge.Client
	// namespace is the provisioned repo's agent-branch prefix (R8),
	// learned from its forge-lint config at provision and restored from
	// the marker on restart.  "" = unknown; only the forge enforces.
	namespace string
}

// LifecycleState is the workspace's disk-derived condition.
type LifecycleState string

const (
	// StateEmpty: the workspace is absent or an empty directory —
	// provision's precondition.
	StateEmpty LifecycleState = "empty"
	// StateProvisioned: tree/.git and the provenance marker are both
	// present — the verbs operate.
	StateProvisioned LifecycleState = "provisioned"
	// StateCorrupt: neither empty nor cleanly provisioned — a mounted host
	// tree (no marker), or a promote interrupted before the marker.  Every
	// verb refuses; recovery is host-side.
	StateCorrupt LifecycleState = "corrupt"
)

// markerName is the provenance marker inside .git — provision's last write
// and dispose's precondition.  Inside .git, so no worktree verb can touch
// it (set_aside cannot stash it, checkpoint cannot commit it), and a
// mounted host tree never carries it.
const markerName = "cloister-grange"

// Cloner performs the hardened clone of repoURL into dst on the default
// branch.  Injected so a test can clone a local bare repo (which the
// endpoint table's https-only allowlist would never admit as a real
// remote); production wires the relay-and-credential clone.
type Cloner func(ctx context.Context, ep endpoint.Endpoint, repoURL, dst string) error

// ProvisionGate verifies a repository's forge protections before its
// grange is handed over (grange.md, "Provision-time verification").
// Verify reads the repo's own forge-lint config from the staging checkout
// and runs the bot-credential check; a non-nil error is a refusal whose
// message names the failing requirement and the lock-down runbook.  It is
// injected because the check lives above this package (it speaks
// forgelint and HTTP); archive only knows to run it and refuse.
type ProvisionGate interface {
	// Verify returns the repository's own agent-branch namespace (R8's
	// `agentNamespace`, e.g. "agent/") so the archivist can refuse an
	// out-of-namespace branch LOCALLY, at start_work, instead of letting
	// the forge reject it at publish — after the agent has already
	// committed work to a doomed branch.  "" means the namespace is
	// unknown and only the server-side rule applies.
	Verify(ctx context.Context, ep endpoint.Endpoint, repo, stagingTree string) (namespace string, err error)
}

// GrangeConfig wires a Grange.  Root, Table, and Gate are required.
type GrangeConfig struct {
	Root  string
	Table *endpoint.Table
	Gate  ProvisionGate
	// Disclosure gates provision on an acknowledgment that this cell's
	// source leaves the machine (internal/disclosure).  Unlike Gate it is
	// OPTIONAL and runs BEFORE the clone: it needs only the repository's
	// name, and refusing here means a repository we have decided not to
	// permit is never fetched at all — as well as failing in a second
	// rather than after a multi-minute clone.  nil -> no such gate, which
	// is every cell that sends its source nowhere.
	Disclosure func(repo string) error
	// OpenForge builds the endpoint's PR-verb client when a workspace
	// opens; nil -> no forge client, and the PR verbs refuse.
	OpenForge func(endpoint.Endpoint) (forge.Client, error)
	GitPath   string           // "" -> "git"
	Now       func() time.Time // nil -> time.Now
	// DefaultBranch overrides origin/HEAD detection; empty is the normal
	// case (a clone always has origin/HEAD).
	DefaultBranch string
	// Cloner overrides the production relay clone; nil -> relay clone.
	Cloner Cloner
}

// ErrNotProvisioned reports a verb invoked on an EMPTY workspace: provision
// must bring a checkout into being first.
var ErrNotProvisioned = errors.New("archive: no grange is provisioned — provision first")

// ErrCorruptWorkspace reports a workspace that is neither empty nor cleanly
// provisioned: recovery is host-side.
var ErrCorruptWorkspace = errors.New("archive: the workspace is neither empty nor cleanly provisioned (no provenance marker) — recover it host-side")

// ErrNotEmpty reports a provision onto a workspace that is not EMPTY —
// grange invariant 3 (never revive a stale volume).
var ErrNotEmpty = errors.New("archive: provision requires an EMPTY workspace")

// ErrAdopted reports a lifecycle verb (provision, dispose, state) called on
// a grange built by AdoptArchive: it wraps a fixed checkout and owns no
// root, so it has no lifecycle to drive.
var ErrAdopted = errors.New("archive: this grange wraps a fixed checkout (AdoptArchive); lifecycle verbs are unavailable")

// NewGrange builds the lifecycle owner over a grange root.  It removes any
// staging left by a crashed provision (always safe — staging is
// disposable) and, when the tree is already provisioned, opens the Archive
// so the verbs work immediately after a restart.
func NewGrange(cfg GrangeConfig) (*Grange, error) {
	switch {
	case cfg.Root == "":
		return nil, fmt.Errorf("archive: grange needs a root directory")
	case cfg.Table == nil:
		return nil, fmt.Errorf("archive: grange needs an endpoint table")
	case cfg.Gate == nil:
		return nil, fmt.Errorf("archive: grange needs a provision gate")
	}
	hooks, err := os.MkdirTemp("", "cloister-grange-hooks-")
	if err != nil {
		return nil, fmt.Errorf("archive: grange hooks dir: %w", err)
	}
	g := &Grange{
		root:       cfg.Root,
		tree:       filepath.Join(cfg.Root, "tree"),
		staging:    filepath.Join(cfg.Root, "staging"),
		git:        orDefault(cfg.GitPath, "git"),
		hooks:      hooks,
		now:        cfg.Now,
		table:      cfg.Table,
		gate:       cfg.Gate,
		disclosure: cfg.Disclosure,
		openForge:  cfg.OpenForge,
		cloner:     cfg.Cloner,
		defBranch:  cfg.DefaultBranch,
	}
	if g.now == nil {
		g.now = time.Now
	}
	if g.cloner == nil {
		g.cloner = g.cloneRelay
	}
	// A staging dir at boot is the residue of a crash mid-provision; the
	// exported tree is untouched, so this is free to discard.
	if err := os.RemoveAll(g.staging); err != nil {
		g.Close()
		return nil, fmt.Errorf("archive: clearing stale staging: %w", err)
	}
	st, err := g.state()
	if err != nil {
		g.Close()
		return nil, err
	}
	if st == StateProvisioned {
		if err := g.open(); err != nil {
			g.Close()
			return nil, fmt.Errorf("archive: adopting the provisioned workspace: %w", err)
		}
	}
	return g, nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// state reads the workspace's condition from disk.
func (g *Grange) state() (LifecycleState, error) {
	if fi, err := os.Stat(filepath.Join(g.tree, ".git")); err == nil && fi.IsDir() {
		if _, err := os.Stat(g.markerPath()); err == nil {
			return StateProvisioned, nil
		}
		// A .git with no marker is a host tree or a promote that died
		// before its last write — never something to empty on a whim.
		return StateCorrupt, nil
	}
	entries, err := os.ReadDir(g.tree)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return StateEmpty, nil
	case err != nil:
		return "", fmt.Errorf("archive: reading workspace state: %w", err)
	case len(entries) == 0:
		return StateEmpty, nil
	}
	return StateCorrupt, nil
}

// State reports the workspace's disk-derived condition.
func (g *Grange) State() (LifecycleState, error) {
	if g.root == "" {
		return "", ErrAdopted
	}
	return g.state()
}

// Status is what the workspace's OWNER needs to decide what to do next:
// the state plus, when provisioned, the provenance behind it.
type Status struct {
	State  LifecycleState
	Repo   string // owner/name; empty unless provisioned
	Branch string // empty on the default branch, or when not provisioned
	// Provisioned is when the marker was written, epoch seconds; zero
	// unless provisioned.
	Provisioned int64
}

// Status reports the state and, when provisioned, the marker behind it —
// so a session manager can say "op/repo on agent/brisk-otter" rather
// than just "provisioned".  A CORRUPT workspace answers its state with
// no provenance: there is none to read, which is what makes it corrupt.
func (g *Grange) Status() (Status, error) {
	if g.root == "" {
		return Status{}, ErrAdopted
	}
	st, err := g.state()
	if err != nil {
		return Status{}, err
	}
	s := Status{State: st}
	if st != StateProvisioned {
		return s, nil
	}
	m, err := g.readMarker()
	if err != nil {
		return Status{}, err
	}
	s.Repo, s.Branch, s.Provisioned = m.Repo, m.Branch, m.Provisioned
	return s, nil
}

func (g *Grange) markerPath() string { return filepath.Join(g.tree, ".git", markerName) }

// Archive returns the live workspace, or a typed error naming why there is
// none (EMPTY vs CORRUPT) so the caller can advise the agent precisely.
func (g *Grange) Archive() (*Archive, error) {
	if g.arc != nil {
		return g.arc, nil
	}
	if st, _ := g.state(); st == StateCorrupt {
		return nil, ErrCorruptWorkspace
	}
	return nil, ErrNotProvisioned
}

// Forge returns the live endpoint's PR-verb client, or an error when the
// workspace is unprovisioned or its endpoint has no forge adapter.
func (g *Grange) Forge() (forge.Client, error) {
	if g.arc == nil {
		return nil, ErrNotProvisioned
	}
	if g.forge == nil {
		name := "this endpoint"
		if ep := g.arc.Endpoint(); ep != nil {
			name = ep.Name
		}
		return nil, fmt.Errorf("archive: %s has no PR-verb adapter", name)
	}
	return g.forge, nil
}

// AdoptArchive wraps an already-open Archive as a live (PROVISIONED)
// grange that serves the working-tree and remote verbs on it, with no
// root, clone, or gate — the seam higher layers' tests use to drive the
// verb surface against a hand-built checkout.  It owns no root, so the
// lifecycle verbs (Provision, Dispose, State) fail fast with ErrAdopted
// rather than acting on a "" path; production granges come from NewGrange.
func AdoptArchive(a *Archive) *Grange {
	return &Grange{arc: a}
}

// AdoptForge wires a PR-verb client into an adopted grange — the seam
// higher layers' tests use to drive the forge verbs against a fake.
// Production granges get their client from OpenForge when the workspace
// opens.
func (g *Grange) AdoptForge(fc forge.Client) {
	g.forge = fc
}

// open opens the Archive at the promoted tree and, when wired, its forge
// client.  The tree must be a real checkout (post-clone or a restart).
func (g *Grange) open() error {
	// On a restart the marker is the only memory of what provision
	// learned, so recover the namespace from it before opening; a fresh
	// provision has already set it and the marker is not written yet.
	if g.namespace == "" {
		if m, err := g.readMarker(); err == nil {
			g.namespace = m.Namespace
		}
	}
	opts := []Option{WithEndpoints(g.table), WithClock(g.now), WithGitPath(g.git), WithBranchNamespace(g.namespace)}
	if g.defBranch != "" {
		opts = append(opts, WithDefaultBranch(g.defBranch))
	}
	a, err := New(g.tree, opts...)
	if err != nil {
		return err
	}
	g.arc = a
	if g.openForge != nil {
		fc, err := g.openForge(*a.Endpoint())
		if err != nil {
			a.Close()
			g.arc = nil
			return err
		}
		g.forge = fc
	}
	return nil
}

// cloneRelay is the production Cloner: the hardened clone of the canonical
// URL, the endpoint's credential and wire mapping riding as the overlay.
func (g *Grange) cloneRelay(ctx context.Context, ep endpoint.Endpoint, repoURL, dst string) error {
	token, err := ep.Token()
	if err != nil {
		return err
	}
	return hardenedClone(ctx, g.git, g.hooks, g.now, endpointOverlay(ep, token), repoURL, dst)
}

// ProvisionInfo says what a provision established.
type ProvisionInfo struct {
	Repo     string // owner/name
	Branch   string // "" when provisioned on the default branch
	Endpoint string
}

// Provision brings an EMPTY workspace into being: resolve the repo against
// the table, clone into staging, gate the clone against the repository's
// protections, then — only on success — promote staging to the exported
// tree, set the line of work and the repo-local identity, and write the
// provenance marker last.  A refusal or any failure before the marker
// leaves the workspace EMPTY (staging discarded, the promoted tree rolled
// back), so provision is idempotent to retry.
func (g *Grange) Provision(ctx context.Context, repoURL string, branch BranchName) (ProvisionInfo, error) {
	if g.root == "" {
		return ProvisionInfo{}, ErrAdopted
	}
	st, err := g.state()
	if err != nil {
		return ProvisionInfo{}, err
	}
	if st != StateEmpty {
		return ProvisionInfo{}, fmt.Errorf("%w (state: %s)", ErrNotEmpty, st)
	}
	ep, err := g.table.ForRemote(repoURL)
	if err != nil {
		return ProvisionInfo{}, err
	}
	repo, err := repoName(ep, repoURL)
	if err != nil {
		return ProvisionInfo{}, err
	}
	info := ProvisionInfo{Repo: repo, Endpoint: ep.Name}

	// Before the clone, deliberately.  The disclosure gate asks whether
	// this repository's source may leave the machine, and the answer does
	// not depend on anything inside the checkout — so a refusal should not
	// have fetched it first, and should not cost the operator a
	// multi-minute clone to learn.  It is also the last point at which
	// nothing has been written: the forge gate below runs after, because
	// it reads the repo's own config out of the staging tree.
	if g.disclosure != nil {
		if err := g.disclosure(repo); err != nil {
			return info, err
		}
	}

	if err := os.RemoveAll(g.staging); err != nil {
		return info, fmt.Errorf("archive: provision: clearing staging: %w", err)
	}
	if err := g.cloner(ctx, ep, repoURL, g.staging); err != nil {
		os.RemoveAll(g.staging)
		return info, fmt.Errorf("archive: provision: cloning %s: %w", repo, err)
	}
	// The gate reads the repo's own forge-lint config from the staging
	// checkout; a refusal discards staging and never touches the tree.
	namespace, err := g.gate.Verify(ctx, ep, repo, g.staging)
	if err != nil {
		os.RemoveAll(g.staging)
		return info, err
	}
	g.namespace = namespace
	// Promote.  RemoveAll first so the rename lands on an absent target on
	// every platform (the state is EMPTY, so the tree is absent or empty).
	if err := os.RemoveAll(g.tree); err != nil {
		os.RemoveAll(g.staging)
		return info, fmt.Errorf("archive: provision: clearing the tree for promote: %w", err)
	}
	if err := os.Rename(g.staging, g.tree); err != nil {
		os.RemoveAll(g.staging)
		return info, fmt.Errorf("archive: provision: promoting staging: %w", err)
	}
	// From here a failure has already touched the tree, so unwind it back
	// to EMPTY rather than leave a markerless (CORRUPT) tree behind.
	if err := g.finishProvision(ctx, repo, branch); err != nil {
		g.rollback()
		return info, err
	}
	info.Branch = branch.String()
	return info, nil
}

// finishProvision does the post-promote steps that must all succeed before
// the marker seals the workspace: open, set the line of work, pin the
// repo-local identity, write the marker.
func (g *Grange) finishProvision(ctx context.Context, repo string, branch BranchName) error {
	if err := g.open(); err != nil {
		return fmt.Errorf("archive: provision: opening the promoted workspace: %w", err)
	}
	if !branch.IsZero() {
		has, err := g.arc.RemoteHasBranch(ctx, branch)
		if err != nil {
			return err
		}
		if has {
			err = g.arc.SwitchWork(ctx, branch) // resume the published line of work
		} else {
			err = g.arc.StartWork(ctx, branch) // a new line of work
		}
		if err != nil {
			return err
		}
	}
	if err := g.arc.SetLocalIdentity(ctx); err != nil {
		return err
	}
	if err := g.writeMarker(repo, branch); err != nil {
		return fmt.Errorf("archive: provision: writing the provenance marker: %w", err)
	}
	return nil
}

// rollback returns a partially-provisioned tree to EMPTY: close the
// Archive and remove the tree.  Best-effort — a failure here leaves a
// CORRUPT tree the operator recovers host-side, which is the same floor
// provision already guarantees.
func (g *Grange) rollback() {
	if g.arc != nil {
		g.arc.Close()
		g.arc = nil
		g.forge = nil
	}
	os.RemoveAll(g.tree)
}

// DisposeInfo says what a dispose did.
type DisposeInfo struct {
	Repo         string
	Branch       string
	AlreadyEmpty bool // the workspace was already EMPTY — a no-op
}

// Dispose returns the workspace to EMPTY.  It refuses a workspace with no
// provenance marker regardless of force — the rail that makes dispose
// structurally unable to wipe a mounted host tree — and, unless force,
// refuses while unpublished work exists.  An already-empty workspace is a
// no-op, not an error.
func (g *Grange) Dispose(ctx context.Context, force bool) (DisposeInfo, error) {
	if g.root == "" {
		return DisposeInfo{}, ErrAdopted
	}
	st, err := g.state()
	if err != nil {
		return DisposeInfo{}, err
	}
	switch st {
	case StateEmpty:
		return DisposeInfo{AlreadyEmpty: true}, nil
	case StateCorrupt:
		return DisposeInfo{}, ErrCorruptWorkspace
	}
	m, _ := g.readMarker() // best-effort provenance for the audit record
	info := DisposeInfo{Repo: m.Repo, Branch: m.Branch}

	if !force {
		a, err := g.Archive()
		if err != nil {
			return info, err
		}
		u, err := a.UnpublishedWork(ctx)
		if err != nil {
			return info, err
		}
		if u.Any() {
			return info, &UnpublishedError{Work: u}
		}
	}
	if g.arc != nil {
		g.arc.Close()
		g.arc = nil
		g.forge = nil
	}
	if err := os.RemoveAll(g.tree); err != nil {
		return info, fmt.Errorf("archive: dispose: emptying the workspace: %w", err)
	}
	return info, nil
}

// Close releases the Archive (if open) and the grange's hooks directory.
func (g *Grange) Close() error {
	var err error
	if g.arc != nil {
		err = g.arc.Close()
		g.arc = nil
		g.forge = nil
	}
	if rerr := os.RemoveAll(g.hooks); err == nil {
		err = rerr
	}
	return err
}

// marker is the provenance marker's on-disk shape: which repository and
// line of work, and when (bare epoch seconds, per the ledger convention).
type marker struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch,omitempty"`
	// Namespace is the repo's declared agent-branch prefix (R8), learned
	// from its forge-lint config at provision and kept here so a restart
	// recovers it without another clone.  Absent in markers written
	// before this field existed: unknown, so only the forge enforces.
	Namespace   string `json:"namespace,omitempty"`
	Provisioned int64  `json:"provisioned"`
}

func (g *Grange) writeMarker(repo string, branch BranchName) error {
	b, err := json.Marshal(marker{
		Repo: repo, Branch: branch.String(),
		Namespace: g.namespace, Provisioned: g.now().Unix(),
	})
	if err != nil {
		return err
	}
	return os.WriteFile(g.markerPath(), append(b, '\n'), 0o600)
}

func (g *Grange) readMarker() (marker, error) {
	raw, err := os.ReadFile(g.markerPath())
	if err != nil {
		return marker{}, err
	}
	var m marker
	if err := json.Unmarshal(raw, &m); err != nil {
		return marker{}, fmt.Errorf("archive: parsing the provenance marker: %w", err)
	}
	return m, nil
}

// repoName derives owner/name from a remote URL and its endpoint, trying
// the canonical designation first and the wire form second (a provisioned
// clone may carry either).
func repoName(ep endpoint.Endpoint, remoteURL string) (string, error) {
	if r, err := forge.RepoFromRemote(ep.Canonical, remoteURL); err == nil {
		return r, nil
	}
	r, err := forge.RepoFromRemote(ep.Wire, remoteURL)
	if err != nil {
		return "", fmt.Errorf("archive: %q does not name a repository at endpoint %s", remoteURL, ep.Name)
	}
	return r, nil
}

// UnpublishedWork is what dispose (and, later, the reaper) checks before
// destroying a workspace: everything that would be lost with the volume.
type UnpublishedWork struct {
	Dirty     []FileChange // tracked edits not yet checkpointed
	Untracked []string     // files no checkpoint has recorded
	SetAside  int          // parcels parked by set_aside
	Unpushed  int          // checkpoints on any local branch not yet at the endpoint
}

// Any reports whether anything would be lost.
func (u UnpublishedWork) Any() bool {
	return len(u.Dirty) > 0 || len(u.Untracked) > 0 || u.SetAside > 0 || u.Unpushed > 0
}

// UnpublishedWork surveys the whole repository — not just the checked-out
// branch — for work that destroying the grange would lose.  It is the
// reusable predicate dispose's refusal and the future reaper's rescue path
// both stand on (docs/archivist.md).
func (a *Archive) UnpublishedWork(ctx context.Context) (UnpublishedWork, error) {
	if err := a.guardConfig(ctx); err != nil {
		return UnpublishedWork{}, err
	}
	st, err := a.currentState(ctx)
	if err != nil {
		return UnpublishedWork{}, err
	}
	u := UnpublishedWork{Dirty: st.Dirty, Untracked: st.Untracked, SetAside: st.SetAside}
	// Commits reachable from any local branch but from no remote-tracking
	// ref — unpublished checkpoints across every line of work at once.
	out, err := a.run.out(ctx, "rev-list", "--count", "--branches", "--not", "--remotes")
	if err != nil {
		return UnpublishedWork{}, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return UnpublishedWork{}, fmt.Errorf("archive: unpublished-commit count %q: %w", out, err)
	}
	u.Unpushed = n
	return u, nil
}

// UnpublishedError reports a dispose refused because destroying the grange
// would lose work.  force overrides it.
type UnpublishedError struct {
	Work UnpublishedWork
}

func (e *UnpublishedError) Error() string {
	var parts []string
	if n := len(e.Work.Dirty); n > 0 {
		parts = append(parts, fmt.Sprintf("%d uncommitted change(s)", n))
	}
	if n := len(e.Work.Untracked); n > 0 {
		parts = append(parts, fmt.Sprintf("%d untracked file(s)", n))
	}
	if e.Work.Unpushed > 0 {
		parts = append(parts, fmt.Sprintf("%d unpublished checkpoint(s)", e.Work.Unpushed))
	}
	if e.Work.SetAside > 0 {
		parts = append(parts, fmt.Sprintf("%d set-aside parcel(s)", e.Work.SetAside))
	}
	return "archive: dispose refused — unpublished work would be lost: " + strings.Join(parts, ", ") +
		"; publish it, or dispose with force to discard it"
}

// RemoteHasBranch reports whether the endpoint already carries name (a
// remote-tracking ref exists after the clone) — provision's test for
// resume-vs-start.
func (a *Archive) RemoteHasBranch(ctx context.Context, name BranchName) (bool, error) {
	if name.IsZero() {
		return false, fmt.Errorf("archive: a branch name is required")
	}
	_, code, err := a.run.exit(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options",
		"refs/remotes/"+originRemote+"/"+name.String())
	if err != nil {
		return false, err
	}
	return code == 0, nil
}

// SetLocalIdentity writes the endpoint's bot identity into .git/config as
// the repo-local author, so the agent's own git (post-M3) commits as the
// bot by default.  Harmless to the archivist's own checkpoints, which pin
// the identity on the command line regardless; user.name/user.email are on
// the config allowlist, so this write does not trip the guard.
func (a *Archive) SetLocalIdentity(ctx context.Context) error {
	if err := a.guardConfig(ctx); err != nil {
		return err
	}
	if _, err := a.run.out(ctx, "config", "user.name", a.ident.Name); err != nil {
		return err
	}
	_, err := a.run.out(ctx, "config", "user.email", a.ident.Email)
	return err
}
