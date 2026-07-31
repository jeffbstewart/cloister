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

package archivist

import (
	"context"
	"errors"
	"log"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jeffbstewart/cloister/internal/archive"
	"github.com/jeffbstewart/cloister/internal/audit"
	"github.com/jeffbstewart/cloister/internal/endpoint"
	"github.com/jeffbstewart/cloister/internal/mcpserve"
	"github.com/jeffbstewart/cloister/internal/runid"
)

// The grange lifecycle verbs (docs/archivist.md, "Grange lifecycle"): the
// workspace's boundary events, audited like the remote verbs.  They act on
// the Grange rather than a live Archive — provision is what brings one into
// being — so they register with add, not addArc.

func (s *Server) registerLifecycleTools() {
	s.add(&mcp.Tool{
		Name: "provision",
		Description: "Bring an EMPTY workspace into being: clone the repository through its endpoint, verify its forge protections meet grange service " +
			"(refusing and naming the failing requirement otherwise), check out a line of work, and record provenance.  " +
			"Refused unless the workspace is empty.  Audited.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"repo":   mcpserve.Str("repository URL to clone, e.g. https://github.com/owner/name — must resolve to a configured endpoint"),
				"branch": mcpserve.Str("line of work to check out: a new agent/… branch, or an existing published one to resume; omit to stay on the default branch"),
			},
			Required:             []string{"repo"},
			AdditionalProperties: mcpserve.NoExtras(),
		},
	}, s.provision)

	s.add(&mcp.Tool{
		Name: "dispose",
		Description: "Return the workspace to EMPTY.  Refuses while unpublished work exists — a dirty tree, checkpoints not yet at the endpoint, or set-aside parcels — " +
			"unless force is set, and refuses any workspace with no provenance marker regardless of force.  Audited.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"force": mcpserve.Boolean("discard unpublished work and empty the workspace anyway"),
			},
			AdditionalProperties: mcpserve.NoExtras(),
		},
	}, s.dispose)
}

func (s *Server) provision(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Repo   string `json:"repo"`
		Branch string `json:"branch"`
	}
	if err := mcpserve.Decode(req, &a); err != nil {
		return mcpserve.ErrResult("bad arguments: " + err.Error()), nil
	}
	if a.Repo == "" {
		return mcpserve.ErrResult("bad arguments: a repo URL is required"), nil
	}
	var branch archive.BranchName
	if a.Branch != "" {
		b, err := archive.ParseBranchName(a.Branch)
		if err != nil {
			return mcpserve.ErrResult("bad arguments: " + err.Error()), nil
		}
		branch = b
	}
	info, err := s.cfg.Grange.Provision(ctx, a.Repo, branch)
	s.auditLifecycle("provision", lifecycleDetail(info.Repo, info.Branch, err), lifecycleDecision(err, audit.DecisionProvisioned))
	if err != nil {
		return mcpserve.ErrResult(err.Error()), nil
	}
	return mcpserve.JSONResult(map[string]any{
		"repo": info.Repo, "branch": info.Branch, "endpoint": info.Endpoint, "provisioned": true,
	}), nil
}

func (s *Server) dispose(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Force bool `json:"force"`
	}
	if err := mcpserve.Decode(req, &a); err != nil {
		return mcpserve.ErrResult("bad arguments: " + err.Error()), nil
	}
	info, err := s.cfg.Grange.Dispose(ctx, a.Force)
	// An already-empty workspace is a no-op, not a boundary event — no record.
	if !(err == nil && info.AlreadyEmpty) {
		s.auditLifecycle("dispose", lifecycleDetail(info.Repo, info.Branch, err), lifecycleDecision(err, audit.DecisionDisposed))
	}
	if err != nil {
		return mcpserve.ErrResult(err.Error()), nil
	}
	if info.AlreadyEmpty {
		return mcpserve.JSONResult(map[string]any{"disposed": false, "already_empty": true}), nil
	}
	return mcpserve.JSONResult(map[string]any{"repo": info.Repo, "branch": info.Branch, "disposed": true}), nil
}

// lifecycleDetail builds the audit detail, lifting the failing requirement
// out of a gate refusal so the record names what the repository lacked.
func lifecycleDetail(repo, branch string, err error) audit.LifecycleDetail {
	d := audit.LifecycleDetail{Repo: repo, Branch: branch}
	var gr *GateRefusedError
	if errors.As(err, &gr) {
		d.Requirement = gr.Requirement()
		if d.Repo == "" {
			d.Repo = gr.Repo
		}
	}
	return d
}

// lifecycleDecision folds a lifecycle result into the audit vocabulary:
// the archivist's own refusals (a non-empty or markerless workspace, a
// failed gate, unpublished work) are DecisionLifecycleRefused; a clone or
// filesystem failure is DecisionLifecycleError.
func lifecycleDecision(err error, success audit.Decision) audit.Decision {
	switch {
	case err == nil:
		return success
	case isLifecycleRefusal(err):
		return audit.DecisionLifecycleRefused
	default:
		return audit.DecisionLifecycleError
	}
}

func isLifecycleRefusal(err error) bool {
	var unpublished *archive.UnpublishedError
	var gate *GateRefusedError
	return errors.Is(err, archive.ErrNotEmpty) ||
		errors.Is(err, archive.ErrCorruptWorkspace) ||
		errors.Is(err, endpoint.ErrNotAllowed) ||
		errors.Is(err, ErrNotGrangeReady) ||
		errors.As(err, &unpublished) ||
		errors.As(err, &gate)
}

// auditLifecycle appends one lifecycle record.  Append failure is logged,
// never fatal — the provision or dispose already happened or refused.
func (s *Server) auditLifecycle(op string, d audit.LifecycleDetail, decision audit.Decision) {
	if s.cfg.Audit == nil {
		return
	}
	id, err := runid.New()
	if err != nil {
		log.Printf("archivist: mint lifecycle op id: %v", err)
		return
	}
	rec := audit.New(id, op, decision, 0)
	rec.Detail = &d
	if err := s.cfg.Audit.Append(rec); err != nil {
		log.Printf("archivist: audit append failed: %v", err)
	}
}
