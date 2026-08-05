# Copyright 2026 Jeffrey B. Stewart
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""cloister_door — the request gate on the jailed-claude door.

Loaded by `mitmdump -s` in the abbey's claude-proxy (docs/JAILED_CLAUDE.md).
It is the ENFORCEMENT half of that design: the topology pins one host, and
this addon pins what may be asked of it.

Three refusals and one observation, all of them structural — they read the
request body on the plaintext hop and hold regardless of what the harness or
the model would prefer.  Nothing here is advisory, and nothing here depends
on a settings key: `permissions.deny` was MEASURED inert under
`bypassPermissions`, which is the mode this whole design exists to enable.

  1. The host.  Only api.anthropic.com, taken from the TLS SNI the agent
     presented — the name it believed it was reaching, not a header any hop
     could have rewritten.

  2. The path.  Only /v1/messages.  This one is not optional and it is easy
     to under-rate: api.anthropic.com is also a STORAGE service.  The Files
     and Batches APIs live on that host, so an agent that can send
     authenticated requests through the door could upload the whole grange
     and collect it later from outside, with every other control in this
     design still intact.  A host pin is not containment when the host will
     hold your bytes for you.

  3. Server-side tools.  Claude Code's WebSearch runs on Anthropic's
     infrastructure: the fetch happens after our last hop, so no relay,
     proxy, or capture can see it and the scholar cannot mediate it.  The
     declaration, though, is in the request body in plain sight.  Refusing
     it here is a body inspection, not a heuristic.  Server-side MCP
     connectors (`mcp_servers`) are the same shape of hole and go with it.

  4. And the observation.  M1 is a diagnostic: the log lines below are the
     answer to "what does the harness actually reach for" — every refused
     attempt by name, every distinct tool set the model was offered, and
     the rate-limit headers Anthropic returns.  That last one closes a
     question the harness probe could not: `--debug` produces a log with no
     response headers in it (lifecycle/probe-claude-harness.sh, step 4),
     while this hop sees every header in cleartext by construction.

The denylist in REFUSED_TOOL_TYPES is deliberately a DENYlist, not an
allowlist, and that is an M1 decision rather than a permanent one.  An
allowlist would be stronger, but we do not yet know which tool types Claude
Code declares — some Anthropic-defined types (the text-editor and bash tool
schemas) are server-DEFINED but client-EXECUTED and perfectly safe, and
refusing those would break the harness for no gain.  So M1 refuses the
server-EXECUTED types we know about and LOGS the rest; tightening to an
allowlist is an M2 change to be made against real observations.

ON-HOST EVALUATION, and the one stand-down.  Refusal 3 assumes the scholar
is reachable — in a cell it always is, and it is the sanctioned research
path that makes refusing WebSearch a redirection rather than an amputation.
The on-host prototype runs of this proxy have NO scholar: it is an abbey
service, and a host-run Claude Code cannot dial it.  Refusing there would
leave that session with no research path at all, which is friction bought
for nothing, since a host-run harness is not jailed in the first place.  So
CLOISTER_DOOR_ALLOW_SERVER_TOOLS=1 stands refusal 3 — and ONLY refusal 3 —
down.  The host and path refusals hold unconditionally; they are what makes
the door a door.

The escape hatch is safe for the same structural reason the git-passthrough
file is: the model cannot reach the place the switch lives.  claude-proxy is
an abbey container with no agent inside it and no filesystem the cell
shares, so its environment is the operator's alone.  Every waiver is logged
at WARNING with the tool named, and the addon announces the stand-down once
at startup — a control that is off should say so in the log an operator is
already reading, not wait to be discovered by its absence.
"""

import json
import logging
import os
from typing import Any, Optional

from mitmproxy import http

log = logging.getLogger("cloister.door")

# Operator-only, default-closed: see "ON-HOST EVALUATION" above.  Read once
# at load, so flipping it takes a container restart rather than taking
# effect silently mid-session.
ALLOW_SERVER_TOOLS = os.environ.get("CLOISTER_DOOR_ALLOW_SERVER_TOOLS", "") == "1"

# The one hostname this door serves.  Every other Anthropic hostname simply
# fails to resolve in the cell, so reaching this check at all means either
# the network alias moved or something is dialing by IP.
EXPECTED_HOST = "api.anthropic.com"

# The one path.  Inference, and nothing that stores bytes for later.
ALLOWED_PATHS = frozenset({"/v1/messages"})

# Tool `type` prefixes whose work happens on ANTHROPIC's side of our last
# hop.  Everything past claude-egress is invisible to this stack, so a
# server-executed tool is an egress channel with no capture and no
# mediation.  Prefix-matched because the types are date-versioned
# (`web_search_20250305`), and a new date must not become a new hole.
REFUSED_TOOL_TYPES = (
    "web_search",
    "web_fetch",
    "code_execution",
    "bash_code_execution",
    "text_editor_code_execution",
)

# Client-side tool NAMES that mean the same thing when the type is absent.
REFUSED_TOOL_NAMES = frozenset({"web_search", "web_fetch"})

# Response headers worth reading off the wire once: the authoritative
# rate-limit picture from Anthropic's side, rather than one reconstructed
# from our own token arithmetic.  Substring-matched, lowercased.
RATE_LIMIT_HINTS = ("ratelimit", "rate-limit", "retry-after")


def _error_body(message: str) -> bytes:
    """Shape a refusal like an Anthropic API error so the harness surfaces
    the reason instead of a bare transport failure.  `permission_error` is a
    real error type in that API, and Claude Code prints the message."""
    return json.dumps(
        {
            "type": "error",
            "error": {"type": "permission_error", "message": "cloister door: " + message},
        }
    ).encode()


class Door:
    def __init__(self) -> None:
        # Observation state.  Both are "log each distinct thing once": an
        # agentic loop makes thousands of requests and a per-request line
        # would bury the one line that matters.
        self._seen_tools: set[str] = set()
        self._logged_rate_limit_headers = False
        if ALLOW_SERVER_TOOLS:
            log.warning(
                "CLOISTER_DOOR_ALLOW_SERVER_TOOLS=1 — server-side tools "
                "(web_search, web_fetch, mcp_servers, code execution) are NOT "
                "refused by this door.  Correct for on-host evaluation, where no "
                "scholar exists to redirect research to.  NEVER set it for a cell: "
                "those tools run past our last hop, where nothing here can see or "
                "mediate them."
            )

    # ── refusal ────────────────────────────────────────────────────────────

    def _refuse(self, flow: http.HTTPFlow, message: str) -> None:
        """Answer locally with a 403 and name what was attempted.  The
        request never reaches claude-egress, so it never acquires the real
        credential — the refusal is upstream of the only hop that holds it."""
        log.warning(
            "REFUSED %s %s (sni=%s): %s",
            flow.request.method,
            flow.request.path,
            flow.client_conn.sni or "-",
            message,
        )
        flow.response = http.Response.make(
            403, _error_body(message), {"content-type": "application/json"}
        )

    # ── the request gate ───────────────────────────────────────────────────

    def request(self, flow: http.HTTPFlow) -> None:
        # The SNI is what the AGENT dialed.  Prefer it over the Host header:
        # a header is a claim any hop could have rewritten, while the SNI was
        # chosen by the client before this proxy could touch anything.  Fall
        # back to Host only for a plain-HTTP connection, which should not
        # happen and is worth seeing in the log when it does.
        dialed = flow.client_conn.sni or flow.request.host_header or ""
        host = dialed.split(":", 1)[0].strip().lower()
        if host != EXPECTED_HOST:
            self._refuse(flow, f"host {host or '(none)'} is not {EXPECTED_HOST}")
            return

        path = flow.request.path.split("?", 1)[0]
        if path not in ALLOWED_PATHS:
            # Naming the path is the point: M1 wants to know what the harness
            # reached for, and a silent 403 answers nothing.
            self._refuse(
                flow,
                f"path {path} is not allowed through this door "
                f"(allowed: {', '.join(sorted(ALLOWED_PATHS))})",
            )
            return

        body = self._decode(flow)
        if body is None:
            self._refuse(flow, "request body is not a JSON object")
            return

        if body.get("mcp_servers") and not self._waive(flow, "mcp_servers"):
            self._refuse(
                flow,
                "the request declares server-side MCP connectors (`mcp_servers`); "
                "they fetch from beyond our last hop, where nothing here can see "
                "or mediate them — the scholar is the sanctioned research path",
            )
            return

        tools = body.get("tools")
        if not isinstance(tools, list):
            tools = []
        for tool in tools:
            if not isinstance(tool, dict):
                continue
            ttype = str(tool.get("type") or "")
            name = str(tool.get("name") or "")
            if not (ttype.startswith(REFUSED_TOOL_TYPES) or name in REFUSED_TOOL_NAMES):
                continue
            if self._waive(flow, ttype or name):
                continue
            self._refuse(
                flow,
                f"the request declares the server-side tool "
                f"{ttype or name!r} — it would execute on Anthropic's side "
                "of this door, unseen and unmediated; research goes through "
                "the scholar",
            )
            return

        self._observe(flow, body, tools)

    def _waive(self, flow: http.HTTPFlow, what: str) -> bool:
        """Report whether the operator has stood the server-side-tool refusal
        down, logging each waiver by name.  A control that is off must leave
        the same trail a control that fired would — otherwise the difference
        between "no server-side tools were attempted" and "we stopped
        checking" is invisible in the log."""
        if not ALLOW_SERVER_TOOLS:
            return False
        log.warning(
            "WAIVED server-side tool %r on %s (CLOISTER_DOOR_ALLOW_SERVER_TOOLS=1)",
            what,
            flow.request.path,
        )
        return True

    # ── observation ────────────────────────────────────────────────────────

    def _observe(self, flow: http.HTTPFlow, body: dict, tools: list) -> None:
        """One line per allowed request, plus a line the first time a new
        tool set appears.  Between them they answer the M1 questions: how
        big is the system prompt, what is the harness offering the model,
        and is it streaming."""
        declared = sorted(
            {str(t.get("type") or t.get("name") or "?") for t in tools if isinstance(t, dict)}
        )
        fresh = [d for d in declared if d not in self._seen_tools]
        if fresh:
            self._seen_tools.update(fresh)
            log.info("tools first seen: %s", ", ".join(fresh))
        log.info(
            "ALLOW %s %s model=%s stream=%s tools=%d body=%dB",
            flow.request.method,
            flow.request.path,
            body.get("model", "?"),
            bool(body.get("stream")),
            len(tools),
            len(flow.request.content or b""),
        )

    def responseheaders(self, flow: http.HTTPFlow) -> None:
        if flow.response is None:
            return

        # Stream the SSE body through instead of buffering it.  Without this
        # mitmproxy holds the whole response until the generation completes,
        # and the agent looks hung for minutes at a time.  It is also why
        # response MUTATION (M2) has to be handled here rather than in the
        # `response` hook — by then a streamed body is gone.
        if "text/event-stream" in flow.response.headers.get("content-type", ""):
            flow.response.stream = True

        if not self._logged_rate_limit_headers:
            seen = {
                k: v
                for k, v in flow.response.headers.items(multi=True)
                if any(hint in k.lower() for hint in RATE_LIMIT_HINTS)
            }
            # Log once either way: "none present" is itself the answer to the
            # question probe step 4 could not settle, and a silent absence
            # would read as "we forgot to look".
            self._logged_rate_limit_headers = True
            log.info("rate-limit headers on first response: %s", seen or "(none present)")

    # ── helpers ────────────────────────────────────────────────────────────

    @staticmethod
    def _decode(flow: http.HTTPFlow) -> Optional[dict[str, Any]]:
        """Parse the request body, or None if it is not a JSON object.  A
        body we cannot read is a body we cannot gate, so the caller refuses
        it rather than passing it through unexamined."""
        try:
            parsed = json.loads(flow.request.content or b"")
        except (ValueError, UnicodeDecodeError):
            return None
        return parsed if isinstance(parsed, dict) else None


addons = [Door()]
