// Register the builder, scribe, AND scholar MCP servers in qwen-code's settings
// on container start, so build/test, the audited write path, and web research are
// available with no per-cell manual step.
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

// tools.core: the capability ALLOWLIST. Only these built-ins are offered to
// the model, so the file mutators (Edit / WriteFile / NotebookEdit) are DISABLED
// and every source write must go through the scribe MCP. Reads, navigation,
// shell, planning, and (vetted) Skills are permitted; web/cron/loop/monitor/
// computer-use/worktree/sub-agent-orchestration/tool-search are denied —
// default-deny: a tool not on this list is unavailable. This is a platform-
// managed security control, OVERWRITTEN each start like the MCP entries. If the
// running agent's tool names differ, override the whole set via QWEN_CORE_TOOLS
// (comma-separated). Add a memory tool here if one appears.
const defaultCoreTools = [
  // read & navigate (reads unaudited; contained by the :ro mount + no egress)
  'ReadFile', 'ListFiles', 'Glob', 'Grep', 'ReadMcpResource',
  // shell (the jail contains it: no egress, source :ro, non-root, ro rootfs)
  'Shell',
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
      `scholar -> ${scholarUrl}; ` +
      `tools.core allowlist = ${coreTools.length} tools (mutators + web excluded); ` +
      `skills = ${allowedSkills.join(', ')}`,
  );
} catch (e) {
  console.error(`qwen-mcp-init: skipped (${e.message})`);
  process.exit(0);
}
