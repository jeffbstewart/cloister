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

// The agent's fail-closed boot self-check — the scholar's pattern
// (internal/scholar/selfcheck.go), ported to the workbench: the network
// jail is effective, or the container refuses to start.  NEGATIVE-ONLY
// probes: success means reaching a public endpoint or resolving a public
// name, and success is fatal.  A slow or refused probe is the healthy
// case, so total boot cost is bounded by the timeouts (~3s worst case,
// probes run concurrently).
//
// The probe runs at BOOT only.  The airlock (bin/update-gradle-deps.bat)
// legitimately connects this container to an egress network mid-life —
// with no agent session running, by the airlock's own gate — and that
// does not restart the container.  A container that RESTARTS while an
// aborted airlock left it connected lands here and refuses: exactly
// right, that airlock must be closed by hand.
import { connect } from 'node:net';
import { Resolver } from 'node:dns/promises';

const TIMEOUT_MS = 3000;

// Stable public anycast endpoints, used only to prove the packet route
// out is DEAD.  Same targets as the scholar's check.
const tcpProbes = ['1.1.1.1:443', '8.8.8.8:53'];
// Well-known public names, used only to prove external resolution is
// dead (dns: 127.0.0.1).  Cell-internal names (agency, archivist) are
// answered authoritatively by the embedded resolver and stay working.
const dnsProbes = ['cloudflare.com', 'google.com'];

function tcpReaches(addr) {
  return new Promise((resolve) => {
    const [host, port] = addr.split(':');
    const sock = connect({ host, port: Number(port), timeout: TIMEOUT_MS });
    const done = (reached) => { sock.destroy(); resolve(reached); };
    sock.once('connect', () => done(true));
    sock.once('timeout', () => done(false));
    sock.once('error', () => done(false));
  });
}

async function dnsResolves(name) {
  const r = new Resolver({ timeout: TIMEOUT_MS, tries: 1 });
  try {
    const addrs = await r.resolve4(name);
    return addrs.length > 0 ? addrs[0] : false;
  } catch {
    return false;
  }
}

const failures = [];
await Promise.all([
  ...tcpProbes.map(async (addr) => {
    if (await tcpReaches(addr)) {
      failures.push(`reached public ${addr} — the agent must have no route to the internet`);
    }
  }),
  ...dnsProbes.map(async (name) => {
    const addr = await dnsResolves(name);
    if (addr) {
      failures.push(`resolved public name "${name}" (${addr}) — external DNS must be dead (dns: 127.0.0.1)`);
    }
  }),
]);

if (failures.length > 0) {
  console.error('*** JAIL SELF-CHECK FAILED — REFUSING TO START ***');
  for (const f of failures) console.error(`  ${f}`);
  console.error('The network jail is not effective.  Check the compose topology');
  console.error('(networks internal, dns pinned) and whether an aborted airlock');
  console.error('left this container connected to an egress network:');
  console.error('  docker network disconnect bridge <project>-agent');
  process.exit(1);
}
console.error('jail-probe: no public egress, no external DNS — jail effective.');
