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

// Registers the cell's MCP servers — the archivist (version control and
// the PR flow) and the scholar (grounded web research) — into qwen-code's
// settings on container start, so no per-cell manual step is needed.
//
// It MERGES: only the platform-managed entries are set or scrubbed; every
// other setting, MCP server, and the user's history are preserved.  The
// workbench agent keeps its NATIVE file tools and shell (it edits and
// builds directly in the grange — docs/grange.md M3/M4); only the
// built-in web tools stay excluded, because web access routes through the
// scholar and the cell has no route for anything else.  Best-effort: any
// failure leaves existing config untouched and the agent still starts.
import { readFileSync, writeFileSync, mkdirSync, existsSync } from 'node:fs';
import { dirname } from 'node:path';

const path = process.env.QWEN_SETTINGS_PATH || '/home/agent/.qwen/settings.json';
const archivistUrl = process.env.ARCHIVIST_MCP_URL || 'http://archivist:9600/mcp';
// await_review BLOCKS up to its maxWait (1h cap) while the operator
// reviews; the MCP timeout must exceed it.  90 min leaves headroom.
const archivistTimeout = Number(process.env.ARCHIVIST_MCP_TIMEOUT || 5400000);
const scholarUrl = process.env.SCHOLAR_MCP_URL || 'http://scholar:9500/mcp';
// research() BLOCKS through the query gate + loop + answer gate (15+10+10
// min, each with its own client-side timeout).  40 min bounds it with
// headroom; the gates return before this fires.
const scholarTimeout = Number(process.env.SCHOLAR_MCP_TIMEOUT || 2400000);

// The built-in web tools stay excluded — web access is the scholar's,
// grounded and gated; a direct fetch has no route anyway and would only
// waste the model's turns.  Override via QWEN_EXCLUDE_TOOLS (comma-
// separated) — a deliberate, reviewable act.
const defaultExcludeTools = ['WebFetch', 'WebSearch'];
const excludeTools = process.env.QWEN_EXCLUDE_TOOLS
  ? process.env.QWEN_EXCLUDE_TOOLS.split(',').map((s) => s.trim()).filter(Boolean)
  : defaultExcludeTools;

// Vetted skills, forward-compat only (the image trim is the authoritative
// control; see the Dockerfile).  Keep in sync with the trim.
const defaultAllowedSkills = ['qc-helper', 'review', 'simplify', 'stuck', 'dataviz', 'new-app'];
const allowedSkills = process.env.QWEN_ALLOWED_SKILLS
  ? process.env.QWEN_ALLOWED_SKILLS.split(',').map((s) => s.trim()).filter(Boolean)
  : defaultAllowedSkills;

try {
  let cfg = {};
  if (existsSync(path)) {
    try {
      cfg = JSON.parse(readFileSync(path, 'utf8'));
    } catch {
      // Unparseable existing settings: never clobber data we can't understand.
      console.error(`qwen-mcp-init: ${path} is not valid JSON; leaving it untouched.`);
      process.exit(0);
    }
  }
  if (cfg === null || typeof cfg !== 'object' || Array.isArray(cfg)) {
    console.error('qwen-mcp-init: settings root is not a JSON object; leaving it untouched.');
    process.exit(0);
  }

  cfg.mcpServers =
    cfg.mcpServers && typeof cfg.mcpServers === 'object' && !Array.isArray(cfg.mcpServers)
      ? cfg.mcpServers
      : {};
  // `trust: true` bypasses qwen's client-side tool-approval DIALOG for a
  // server — never its server-side gates.  The scholar keeps its own
  // query + answer human gates, far stronger than a tool-name prompt.
  // The archivist stays UNtrusted: its verbs move history and publish
  // work, so the dialog stays on unless the operator chooses YOLO.
  cfg.mcpServers.archivist = { httpUrl: archivistUrl, timeout: archivistTimeout };
  cfg.mcpServers.scholar = { httpUrl: scholarUrl, timeout: scholarTimeout, trust: true };
  // Scrub the retired mediators' managed entries from any earlier life:
  // a stale server would only offer the model dead endpoints.
  delete cfg.mcpServers.builder;
  delete cfg.mcpServers.scribe;
  delete cfg.mcpServers.librarian;

  // The workbench agent works with its native tools: scrub the old
  // mediation-era core-tools ALLOWLIST (which disabled the file tools and
  // shell) and manage only the web-tool exclusion.  Merge so any sibling
  // tools.* settings survive.
  cfg.tools =
    cfg.tools && typeof cfg.tools === 'object' && !Array.isArray(cfg.tools) ? cfg.tools : {};
  delete cfg.tools.core;
  delete cfg.coreTools; // the legacy flat key, if an old run left one
  cfg.tools.exclude = excludeTools;
  // Forward-compat skills allowlist; the image trim is authoritative.
  cfg.skills =
    cfg.skills && typeof cfg.skills === 'object' && !Array.isArray(cfg.skills) ? cfg.skills : {};
  cfg.skills.allowed = allowedSkills;
  delete cfg.allowedSkills;

  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, JSON.stringify(cfg, null, 2) + '\n');
  console.error(
    `qwen-mcp-init: registered archivist -> ${archivistUrl} (prompts unless YOLO), ` +
      `scholar -> ${scholarUrl} (trusted); native tools enabled, excluded = ${excludeTools.join(', ')}; ` +
      `skills = ${allowedSkills.join(', ')}`,
  );
} catch (e) {
  console.error(`qwen-mcp-init: skipped (${e.message})`);
  process.exit(0);
}
