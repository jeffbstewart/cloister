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

// The claude variant's boot self-check, run after jail-probe.mjs and before
// anything else (docs/JAILED_CLAUDE.md).  jail-probe proves the packet route
// out is dead; this proves the ONE route that is deliberately alive goes
// where we think it goes.
//
// The design's second guarantee is a claim about trust, not topology: "no
// route exists to anything but the proxy, AND no issuer is trusted but
// ours."  The first half is enforced by compose-lint on a file.  The second
// half is an assertion about what is inside this image — and an image
// assertion that nothing checks at runtime is exactly the kind of control
// the deny-rule finding taught us to distrust.  So it is checked here, on
// every boot, against the store as it actually exists.
//
// TWO checks, and they fail differently ON PURPOSE:
//
//   1. The trust store holds exactly ONE certificate.  Image invariant,
//      always checkable, depends on nothing else.  FATAL — a store that has
//      regrown the public roots has silently lost the guarantee, and the
//      container looks completely normal otherwise.
//
//   2. api.anthropic.com answers with a certificate that store validates.
//      This one depends on ANOTHER STACK being up, so it distinguishes:
//      - handshake succeeded          -> the door is ours.  Good.
//      - certificate rejected         -> FATAL.  Something is answering to
//        that name that our CA did not sign.  Either the alias moved or
//        this cell has a route it should not have; both are containment
//        failures and neither should start an agent.
//      - could not connect at all     -> WARN, not fatal.  The abbey door
//        is probably just down.  Refusing to boot on another stack's
//        availability turns a fixable outage into a restart loop, and the
//        agent will get a clear connection error the moment it tries.

import { readFileSync } from 'node:fs';
import { connect } from 'node:tls';

const STORE = '/etc/ssl/certs/ca-certificates.crt';
const DOOR_HOST = 'api.anthropic.com';
const DOOR_PORT = 443;
const TIMEOUT_MS = 3000;

const fatal = [];
const warn = [];

// ── 1 ── the trust store holds exactly one certificate ──────────────────────
let store = null;
try {
  store = readFileSync(STORE);
  const n = (store.toString().match(/BEGIN CERTIFICATE/g) || []).length;
  if (n !== 1) {
    fatal.push(
      `the system trust store (${STORE}) holds ${n} certificates, not 1 — ` +
        'the cell is meant to trust the cloister CA and nothing else, so that ' +
        'a route to any real internet host fails validation loudly'
    );
  }
} catch (e) {
  fatal.push(`cannot read the system trust store (${STORE}): ${e.message}`);
}

// ── 2 ── the door presents a certificate that store validates ───────────────
// Deliberately NOT a plain "can I connect" probe.  Reachability proves
// nothing here; the question is whether the thing answering is ours.
function probeDoor() {
  return new Promise((resolve) => {
    if (store === null) return resolve({ kind: 'skipped' });
    const sock = connect(
      {
        host: DOOR_HOST,
        port: DOOR_PORT,
        servername: DOOR_HOST,
        // The store as it actually is on disk, not node's bundled Mozilla
        // set — the bundled set is what CLAUDE_CODE_CERT_STORE=system exists
        // to bypass, so probing against it would test the wrong thing.
        ca: store,
        rejectUnauthorized: true,
        timeout: TIMEOUT_MS,
      },
      () => {
        const cert = sock.getPeerCertificate();
        sock.destroy();
        resolve({ kind: 'ok', issuer: cert?.issuer?.CN || '(unnamed issuer)' });
      }
    );
    sock.once('timeout', () => {
      sock.destroy();
      resolve({ kind: 'unreachable', why: `no answer within ${TIMEOUT_MS}ms` });
    });
    sock.once('error', (e) => {
      sock.destroy();
      // A TLS verification failure is a different animal from a dead socket.
      // node reports the former through `code` values that start with the
      // openssl reason or the depth-0 shorthand; everything else here is a
      // transport problem.
      const transport = ['ECONNREFUSED', 'ENOTFOUND', 'EHOSTUNREACH', 'ENETUNREACH', 'EAI_AGAIN'];
      if (transport.includes(e.code)) {
        resolve({ kind: 'unreachable', why: `${e.code}` });
      } else {
        resolve({ kind: 'rejected', why: e.code ? `${e.code}: ${e.message}` : e.message });
      }
    });
  });
}

const door = await probeDoor();
switch (door.kind) {
  case 'ok':
    console.error(`claude-door-probe: ${DOOR_HOST} answered with a certificate issued by "${door.issuer}" — the door is ours.`);
    break;
  case 'rejected':
    fatal.push(
      `${DOOR_HOST} answered with a certificate this cell does NOT trust (${door.why}) — ` +
        'either the network alias no longer points at claude-proxy, or this cell ' +
        'has a route to the real internet.  Both are containment failures'
    );
    break;
  case 'unreachable':
    warn.push(
      `${DOOR_HOST} did not answer (${door.why}) — the abbey's claude door is ` +
        'probably down.  Not refusing the boot over another stack, but no ' +
        'inference will work until it is back: check claude-proxy and claude-egress'
    );
    break;
}

for (const w of warn) console.error(`claude-door-probe: WARNING ${w}`);

if (fatal.length > 0) {
  console.error('*** CLAUDE DOOR SELF-CHECK FAILED — REFUSING TO START ***');
  for (const f of fatal) console.error(`  ${f}`);
  console.error('The second guarantee behind this design is that the cell trusts exactly');
  console.error('one issuer.  Without it the topology is holding the line alone, which is');
  console.error('precisely the situation the trust store exists to cover.');
  process.exit(1);
}
console.error('claude-door-probe: one trusted issuer, and the door is behind it.');
