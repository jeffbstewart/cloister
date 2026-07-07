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

// Register the builder, scribe, scholar, AND librarian MCP servers in
// qwen-code's settings on container start, so build/test, the audited write
// path, web research, and the shield-filtered read path are available with no
// per-cell manual step.
//
// It MERGES: only the platform-managed `builder` and `scribe` entries are
// set/overwritten; every other setting, MCP server, and the user's history are
// preserved. URLs default to the cell stack's project-agnostic service names,
// overridable via env. Best-effort: any failure leaves existing config untouched
// and the agent still starts (the entrypoint ignores this script's exit status).
import { readFileSync, writeFileSync, mkdirSync, existsSync } from 'node:fs';
import { dirname } from 'node:path';

const path = process.env.QWEN_SETTINGS_PATH || '/home/node/.qwen/settings.json';
const builderUrl = process.env.BUILDER_MCP_URL || 'http://builder:9200/mcp';
const scribeUrl = process.env.SCRIBE_MCP_URL || 'http://scribe:9300/mcp';
const timeout = Number(process.env.BUILDER_MCP_TIMEOUT || 2400000);
// The scribe's call BLOCKS while a gated write awaits approval, so its timeout
// must exceed the state service's approval timeout (default 1h) — else the agent's
// call dies before the human decides. 90 min > 1 h leaves headroom.
const scribeTimeout = Number(process.env.SCRIBE_MCP_TIMEOUT || 5400000);
const scholarUrl = process.env.SCHOLAR_MCP_URL || 'http://scholar:9500/mcp';
// research() BLOCKS through the query gate + loop + answer gate (15+10+10
// min, each with its own client-side timeout). 40 min MCP timeout bounds it with
// headroom; the gates return before this fires.
const scholarTimeout = Number(process.env.SCHOLAR_MCP_TIMEOUT || 2400000);
const librarianUrl = process.env.LIBRARIAN_MCP_URL || 'http://librarian:9400/mcp';

// tools.core: the capability ALLOWLIST. Only these built-ins are offered to
// the model. The file mutators (Edit / WriteFile / NotebookEdit), the file
// readers (ReadFile / ListFiles / Grep / Glob), AND Shell are all DISABLED:
// writes go through the scribe MCP, reads through the librarian MCP, and with
// no workspace mount, no toolchain, and no egress a shell has no legitimate
// use left — the built-ins would only show the model an empty world, and a
// general-purpose runtime is pure probing surface once its real jobs are
// mediated. web/cron/loop/monitor/computer-use/worktree/sub-agent-
// orchestration/tool-search are likewise denied — default-deny: a tool not on
// this list is unavailable. This is a platform-managed security control,
// OVERWRITTEN each start like the MCP entries. If a vetted skill genuinely
// needs a shell (or tool names differ), override the whole set via
// QWEN_CORE_TOOLS (comma-separated) — a deliberate, reviewable act.
const defaultCoreTools = [
  // MCP resources only — source reads route via the librarian MCP tools
  'ReadMcpResource',
  // planning / bookkeeping (harness-level, low-risk)
  'AskUserQuestion', 'EnterPlanMode', 'ExitPlanMode', 'TodoList',
  // packaged skills — the tool is permitted; individual skills are vetted below
  'Skill',
];
const coreTools = process.env.QWEN_CORE_TOOLS
  ? process.env.QWEN_CORE_TOOLS.split(',').map((s) => s.trim()).filter(Boolean)
  : defaultCoreTools;

// Vetted skills ("allow only specific vetted skills"). The REAL enforcement
// is presence-on-disk: the agent image (qwen/Dockerfile) ships only these bundled
// skills — qwen-code has no shipped settings allowlist for skills (QwenLM/
// qwen-code#2216 is unmerged). The setting written below is FORWARD-COMPAT only
// (the nested `skills.allowed` form #2216 proposes), harmless if the running
// version ignores it. Keep this list in sync with the Dockerfile trim. Override
// via QWEN_ALLOWED_SKILLS.
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
  cfg.mcpServers.builder = { httpUrl: builderUrl, timeout };
  cfg.mcpServers.scribe = { httpUrl: scribeUrl, timeout: scribeTimeout };
  cfg.mcpServers.scholar = { httpUrl: scholarUrl, timeout: scholarTimeout };
  cfg.mcpServers.librarian = { httpUrl: librarianUrl, timeout };

  // Platform-managed security control: authoritatively set the allowlist so the
  // built-in mutators stay disabled regardless of prior settings. qwen-code nests
  // this under tools.core; the top-level `coreTools` key is legacy and ignored.
  // Merge so any sibling tools.* settings (e.g. tools.exclude) are preserved.
  cfg.tools =
    cfg.tools && typeof cfg.tools === 'object' && !Array.isArray(cfg.tools) ? cfg.tools : {};
  cfg.tools.core = coreTools;
  delete cfg.coreTools; // scrub the legacy flat key an earlier run left behind
  // Forward-compat skills allowlist (#2216 proposes skills.allowed); the image
  // trim is the authoritative control. Merge so any sibling skills.* survive.
  cfg.skills =
    cfg.skills && typeof cfg.skills === 'object' && !Array.isArray(cfg.skills) ? cfg.skills : {};
  cfg.skills.allowed = allowedSkills;
  delete cfg.allowedSkills; // scrub the earlier flat skills key too

  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, JSON.stringify(cfg, null, 2) + '\n');
  console.error(
    `qwen-mcp-init: registered builder -> ${builderUrl}, scribe -> ${scribeUrl}, ` +
      `scholar -> ${scholarUrl}, librarian -> ${librarianUrl}; ` +
      `tools.core allowlist = ${coreTools.length} tools (mutators + readers + shell + web excluded); ` +
      `skills = ${allowedSkills.join(', ')}`,
  );
} catch (e) {
  console.error(`qwen-mcp-init: skipped (${e.message})`);
  process.exit(0);
}

// Materialize the IMAGE-BAKED, project-agnostic QWEN.md into the tmpfs cwd:
// qwen-code auto-loads it from the working directory, which no longer shares
// a filesystem with the project.  A purely local copy — no service ordering,
// no network.  Project-specific guidance stays in the workspace, read via the
// librarian mid-session per the baked prompt's instructions.
const bakedPrompt = process.env.QWEN_BAKED_PROMPT || '/usr/local/share/cloister/QWEN.md';
const promptDest = process.env.QWEN_PROMPT_DEST || '/workspace/QWEN.md';
try {
  if (existsSync(bakedPrompt)) {
    writeFileSync(promptDest, readFileSync(bakedPrompt));
    console.error(`qwen-mcp-init: materialized ${promptDest} from ${bakedPrompt}`);
  } else {
    console.error(`qwen-mcp-init: no baked prompt at ${bakedPrompt}`);
  }
} catch (e) {
  console.error(`qwen-mcp-init: baked prompt not materialized (${e.message}); continuing`);
}
