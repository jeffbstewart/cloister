#!/usr/bin/env bash
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

# claude-door-cert.sh — the two certificate artifacts the jailed-claude door
# needs, and a check that they are actually usable (docs/JAILED_CLAUDE.md).
#
#   claude-leaf.pem   the api.anthropic.com LEAF: private key + certificate +
#                     issuing chain, in the single file mitmproxy's --certs
#                     wants.  Mounted read-only into claude-proxy;
#                     ${CLAUDE_LEAF_CERT} in docker/abbey-claude.yaml.
#   cloister-ca.crt   the issuing CA's CERTIFICATE alone.  Becomes the
#                     cell's entire trust store, and the value of the
#                     CLOISTER_CA_PEM repo variable the image build reads.
#
# THE CA PRIVATE KEY NEVER ENTERS THE STACK.  That is the point of minting a
# leaf here rather than handing mitmproxy a CA to sign with: this door
# impersonates exactly one hostname, forever, so there is nothing for it to
# sign at runtime.  A leaf key compromise forges one name the topology
# already routes to us; a CA key compromise forges every name, against a
# trust store that trusts only that CA, in every cell on the machine.
#
# Use the CA you already have.  Standing up a dedicated one is ceremony —
# the argument for it rested on the CA key living in claude-proxy, and it
# does not.  Two consequences to accept deliberately, both in the design doc:
# the cell trusts everything that CA has signed, and a leaf leak forges
# api.anthropic.com to anything trusting that CA.
#
# Usage (from anywhere in the repo):
#
#   # The firewall's CA manager issued the leaf for you (the common case):
#   bash lifecycle/claude-door-cert.sh assemble \
#     --leaf-cert leaf.crt --leaf-key leaf.key --ca-cert ca.crt \
#     --out /srv/cloister/claude-door
#
#   # ...or you hold the CA key and want this script to issue it:
#   bash lifecycle/claude-door-cert.sh mint \
#     --ca-cert ca.crt --ca-key ca.key --out /srv/cloister/claude-door
#
#   # Check an existing pair before deploying, or after a rotation:
#   bash lifecycle/claude-door-cert.sh verify /srv/cloister/claude-door
#
# On a Windows operator box, invoke Git Bash by full path — a bare `bash`
# resolves to the WSL launcher, which has a different filesystem view:
#   & "C:\Program Files\Git\bin\bash.exe" lifecycle/claude-door-cert.sh verify …
#
# `verify` runs automatically at the end of `mint` and `assemble`.  It is
# also the whole reason this script exists rather than three lines of
# openssl in a runbook: every check below corresponds to a failure that
# otherwise surfaces as an opaque TLS error inside a container, two hops
# from its cause, at the moment an agent first tries to think.

set -euo pipefail

readonly DOOR_HOST="api.anthropic.com"
readonly LEAF_PEM="claude-leaf.pem"
readonly CA_CRT="cloister-ca.crt"

die() { echo "claude-door-cert: $*" >&2; exit 1; }
note() { echo "  $*"; }
rule() { printf '%s\n' "────────────────────────────────────────────────────────────"; }

command -v openssl >/dev/null 2>&1 || die "openssl is not on PATH."

# out_dir validates and creates the destination.  It REFUSES the repository
# tree: this directory is about to hold a private key, and the presubmit
# secret scanner is the last line of defence, not the first.  A refusal here
# costs the operator one flag; the alternative costs a key rotation.
out_dir() {
  local dir="$1"
  [[ -n "$dir" ]] || die "--out is required (a directory OUTSIDE this repository)"
  local repo
  repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
  mkdir -p "$dir"
  local abs
  abs="$(cd "$dir" && pwd -P)"
  case "$abs/" in
    "$repo"/*)
      die "refusing to write a private key inside the repository ($abs).
  Pick a directory the tree does not contain — the mount paths in
  docker/abbey-claude.yaml are host paths, not repo paths."
      ;;
  esac
  printf '%s' "$abs"
}

# ── mint: we hold the CA key ────────────────────────────────────────────────
cmd_mint() {
  local ca_cert="" ca_key="" out="" days=397
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --ca-cert) ca_cert="$2"; shift 2 ;;
      --ca-key)  ca_key="$2";  shift 2 ;;
      --out)     out="$2";     shift 2 ;;
      --days)    days="$2";    shift 2 ;;
      *) die "unknown flag $1 (mint takes --ca-cert --ca-key --out [--days])" ;;
    esac
  done
  [[ -s "$ca_cert" ]] || die "--ca-cert must name a readable file"
  [[ -s "$ca_key"  ]] || die "--ca-key must name a readable file"
  out="$(out_dir "$out")"

  local work; work="$(mktemp -d)"; trap 'rm -rf "$work"' RETURN

  # SAN is not optional and CN is not a substitute: every current TLS client
  # ignores commonName entirely for hostname matching.  A leaf with the right
  # CN and no SAN produces a certificate that looks correct in every viewer
  # and is rejected by the agent.
  cat >"$work/ext" <<EOF
basicConstraints = critical, CA:FALSE
keyUsage = critical, digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = DNS:${DOOR_HOST}
EOF

  # The subject goes in a CONFIG FILE, not in -subj.  On a Windows operator
  # box this script runs under Git Bash, whose MSYS argument conversion sees
  # a leading "/" and helpfully rewrites "/CN=api.anthropic.com" into
  # "C:/Program Files/Git/CN=api.anthropic.com" — openssl then rejects the
  # subject in a message that says nothing about path translation.  A config
  # file has no leading slash to mangle and behaves identically on both
  # platforms, which is worth more than the one line it costs.
  cat >"$work/req.cnf" <<EOF
[req]
distinguished_name = dn
prompt = no
[dn]
CN = ${DOOR_HOST}
EOF

  echo "minting a ${days}-day leaf for ${DOOR_HOST}…"
  openssl req -new -newkey rsa:2048 -nodes \
    -config "$work/req.cnf" \
    -keyout "$work/leaf.key" -out "$work/leaf.csr" 2>/dev/null \
    || die "could not create the signing request"
  # -CAserial into the scratch dir: -CAcreateserial alone drops a .srl file
  # next to the operator's CA, and this script has no business writing into
  # whatever directory that is.
  openssl x509 -req -in "$work/leaf.csr" \
    -CA "$ca_cert" -CAkey "$ca_key" \
    -CAserial "$work/ca.srl" -CAcreateserial \
    -days "$days" -sha256 -extfile "$work/ext" \
    -out "$work/leaf.crt" 2>/dev/null \
    || die "could not sign the leaf — is --ca-key the key for --ca-cert?"

  write_bundle "$work/leaf.crt" "$work/leaf.key" "$ca_cert" "$out"
  cmd_verify "$out"
}

# ── assemble: the firewall's CA manager issued the leaf ─────────────────────
cmd_assemble() {
  local leaf_cert="" leaf_key="" ca_cert="" out=""
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --leaf-cert) leaf_cert="$2"; shift 2 ;;
      --leaf-key)  leaf_key="$2";  shift 2 ;;
      --ca-cert)   ca_cert="$2";   shift 2 ;;
      --out)       out="$2";       shift 2 ;;
      *) die "unknown flag $1 (assemble takes --leaf-cert --leaf-key --ca-cert --out)" ;;
    esac
  done
  [[ -s "$leaf_cert" ]] || die "--leaf-cert must name a readable file"
  [[ -s "$leaf_key"  ]] || die "--leaf-key must name a readable file"
  [[ -s "$ca_cert"   ]] || die "--ca-cert must name a readable file"
  out="$(out_dir "$out")"
  write_bundle "$leaf_cert" "$leaf_key" "$ca_cert" "$out"
  cmd_verify "$out"
}

# write_bundle lays out the two artifacts.  Key first, then leaf, then chain:
# mitmproxy parses either order, and this one matches what every other tool
# expects, so an operator inspecting the file with `openssl x509 -in` gets
# the leaf rather than the CA.
write_bundle() {
  local leaf_cert="$1" leaf_key="$2" ca_cert="$3" out="$4"
  umask 077
  { cat "$leaf_key"; cat "$leaf_cert"; cat "$ca_cert"; } > "$out/$LEAF_PEM"
  umask 022
  cp "$ca_cert" "$out/$CA_CRT"
  chmod 0600 "$out/$LEAF_PEM"
  chmod 0644 "$out/$CA_CRT"
  echo "wrote $out/$LEAF_PEM (0600) and $out/$CA_CRT (0644)"
}

# ── verify: everything that would otherwise fail opaquely in a container ────
cmd_verify() {
  local out="${1:-}"
  [[ -n "$out" ]] || die "verify takes the output directory"
  local pem="$out/$LEAF_PEM" ca="$out/$CA_CRT"
  [[ -s "$pem" ]] || die "$pem is missing or empty"
  [[ -s "$ca"  ]] || die "$ca is missing or empty"

  local work; work="$(mktemp -d)"; trap 'rm -rf "$work"' RETURN
  # The first certificate in the bundle is the leaf (see write_bundle).
  openssl x509 -in "$pem" -out "$work/leaf.crt" 2>/dev/null \
    || die "$pem holds no readable certificate"

  local fail=0
  rule; echo "VERIFY $out"; rule

  # 1. SAN.  The failure this catches is invisible: a cert with the right CN
  #    and no SAN renders perfectly everywhere and is refused by every client.
  local san
  san="$(openssl x509 -in "$work/leaf.crt" -noout -ext subjectAltName 2>/dev/null || true)"
  if [[ "$san" == *"DNS:${DOOR_HOST}"* ]]; then
    note "OK   subjectAltName covers ${DOOR_HOST}"
  else
    note "FAIL subjectAltName does not cover ${DOOR_HOST} — every current TLS"
    note "     client ignores commonName, so this certificate cannot work"
    note "     however correct it looks.  Got: ${san:-<none>}"
    fail=1
  fi

  # 2. The key matches the certificate.  Trivial to get wrong when exporting
  #    from a CA manager, and the symptom is a proxy that refuses to start
  #    with a message about the key, several layers down.
  local kmod cmod
  kmod="$(openssl pkey -in "$pem" -pubout 2>/dev/null | openssl sha256 2>/dev/null || true)"
  cmod="$(openssl x509 -in "$work/leaf.crt" -noout -pubkey 2>/dev/null | openssl sha256 2>/dev/null || true)"
  if [[ -n "$kmod" && "$kmod" == "$cmod" ]]; then
    note "OK   the private key in the bundle matches the certificate"
  else
    note "FAIL the bundle's private key does not match its certificate"
    note "     (or there is no private key in $pem at all)"
    fail=1
  fi

  # 3. The chain verifies against the CA the CELL will trust.  This is the
  #    end-to-end question: the cell's whole trust store is that one file.
  if openssl verify -CAfile "$ca" "$work/leaf.crt" >/dev/null 2>&1; then
    note "OK   the leaf verifies against $CA_CRT — the cell's entire trust store"
  else
    note "FAIL the leaf does NOT verify against $CA_CRT"
    note "     $(openssl verify -CAfile "$ca" "$work/leaf.crt" 2>&1 | tail -1)"
    fail=1
  fi

  # 4. Exactly one certificate in the CA file.  The image build asserts this
  #    too, but failing here costs a second instead of a CI round trip — and
  #    the whole "no issuer is trusted but ours" claim is this count.
  local n
  n="$(grep -c 'BEGIN CERTIFICATE' "$ca" || true)"
  if [[ "$n" == 1 ]]; then
    note "OK   $CA_CRT holds exactly one certificate"
  else
    note "FAIL $CA_CRT holds $n certificates, not 1 — the cell is meant to"
    note "     trust one issuer, and the image build will refuse this"
    fail=1
  fi

  # 5. Permissions on the bundle, which holds a PRIVATE KEY.  write_bundle
  #    chmods it 0600 and that silently does not stick everywhere: under
  #    MSYS on NTFS the call succeeds and the POSIX bits stay 0644, because
  #    the real access control there is the ACL.  So an operator who mints on
  #    a Windows box and copies the result to the Linux host carries a
  #    world-readable key across without a word of warning.
  #
  #    A WARN rather than a FAIL, on purpose: on the box where this most
  #    often happens the file genuinely is protected, so failing would be
  #    crying wolf at the one platform that cannot satisfy the check.
  local mode
  mode="$(stat -c '%a' "$pem" 2>/dev/null || stat -f '%Lp' "$pem" 2>/dev/null || echo "")"
  case "$mode" in
    600|400|"") note "OK   $LEAF_PEM is not readable beyond its owner${mode:+ (0$mode)}" ;;
    *)
      note "WARN $LEAF_PEM is mode 0$mode — it holds a PRIVATE KEY."
      note "     Expected 0600.  On Windows/MSYS the chmod does not take and the"
      note "     ACL is the real control, so this may be fine HERE — but fix it"
      note "     on the host that will actually mount it:  chmod 0600 <file>"
      ;;
  esac

  # 6. Validity window.  Nothing enforces rotation (the design says so
  #    plainly), so the least this can do is say when it will break.
  local notafter
  notafter="$(openssl x509 -in "$work/leaf.crt" -noout -enddate 2>/dev/null | cut -d= -f2-)"
  if openssl x509 -in "$work/leaf.crt" -noout -checkend 0 >/dev/null 2>&1; then
    if openssl x509 -in "$work/leaf.crt" -noout -checkend 2592000 >/dev/null 2>&1; then
      note "OK   valid until $notafter"
    else
      note "WARN valid until $notafter — under 30 days left; re-mint before it"
      note "     lapses, or the cell stops being able to think mid-task"
    fi
  else
    note "FAIL expired on $notafter"
    fail=1
  fi

  rule
  if [[ "$fail" != 0 ]]; then
    echo "NOT USABLE — fix the above before deploying." >&2
    return 1
  fi
  cat <<EOF
USABLE.  Wire it up:

  docker/abbey-claude.yaml   CLAUDE_LEAF_CERT=$out/$LEAF_PEM
  the image build            set the repo variable CLOISTER_CA_PEM to the
                             contents of $out/$CA_CRT
                             (gh variable set CLOISTER_CA_PEM < $out/$CA_CRT)

Neither file belongs in the repository, and $LEAF_PEM least of all.
EOF
}

case "${1:-}" in
  mint)     shift; cmd_mint "$@" ;;
  assemble) shift; cmd_assemble "$@" ;;
  verify)   shift; cmd_verify "$@" ;;
  *)
    sed -n '/^# Usage/,/^# openssl in a runbook/p' "$0" | sed 's/^# \{0,1\}//'
    exit 2
    ;;
esac
