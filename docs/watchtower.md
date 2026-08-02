# The update watcher

Status: **decided and wired 2026-08-02** (docker/watchtower.yaml).  The
problem it solves is not speed — it is that updates across a fleet of
cells are otherwise *forgotten*.

## Problem

The abbey and every cell run images that move: `:latest` advances on
each merge to main.  Docker only pulls a tag it lacks, so a restart
re-runs the cached image and a deployment silently ages.  With one
machine and a handful of services that is a chore; with the cellarer's
4–6 fungible cells it is a chore nobody performs, and the fleet drifts
into running builds whose provenance nobody remembers.

## Shape

A single `containrrr/watchtower`, **pinned to a release**, polling the
daemon and restarting a container when its tag moves.

- **Host infrastructure, never a stack member.**  Watchtower needs
  `/var/run/docker.sock`, which is root-equivalent control of the host:
  with it a container can start a privileged sibling, mount any host
  path, and read every other container's secrets.  So it lives in its
  own stack, on no cloister network, reachable by no worker — the same
  posture as the Portainer agent.  **compose-lint refuses a docker
  socket anywhere in `abbey.yaml` or `cell.yaml`**, permanently and
  independent of watchtower: "no cell member holds the control plane"
  is a load-bearing claim that deserves an enforcer.
- **Opt-in by label.**  `WATCHTOWER_LABEL_ENABLE` means only services
  carrying `com.centurylinklabs.watchtower.enable=true` are considered,
  so a hand-run container — a throwaway ollama, a template probe — is
  never restarted underneath the operator.
- **Pinned, and never self-updating.**  An updater tracking `:latest`
  would swap its own binary with no reviewer in the loop; the one
  container holding the control plane is the last place to accept
  that.  Its version moves when a human edits `WATCHTOWER_IMAGE`.

## What is watched, and what is not

| Service | Watched | Why |
|---|---|---|
| agency, state, scholar (abbey) | yes | bounded operations; they drain |
| archivist (cell) | yes | drains — a clone or poll in flight completes |
| **agent (cell)** | **no** | its unit of work is a human's open-ended session |
| infer, relays, status | no | pinned upstream images; they move on deliberate edits |

**The agent's exclusion is the considered one.**  Every other service
does bounded work: a request arrives, it finishes, the process can
exit between them.  The agent's work is a person's session — arbitrary
length, resumable only by that person, and holding context no restart
preserves.  There is also no external protocol to ask an interactive
CLI to wind up: the container's PID 1 is `sleep infinity`, so a
container SIGTERM never reaches the CLI at all, and the closest thing
to a graceful request is `tmux send-keys` of its own quit command —
which still ends a human's work at an arbitrary moment.

Restarting the agent can never break another service (it holds no
inbound listener; nothing depends on it), so the only cost is
interrupting the operator — which is exactly why the operator should be
the one to choose the moment.  Agent updates ride the natural task
boundary: `dispose`, redeploy, `provision`.

## Interaction with the lame duck

The watched workers drain (docs/abbey.md): SIGTERM stops new
admissions, in-flight work finishes, and the process exits before the
service's `stop_grace_period`.  Watchtower honors that grace period, so
updating an archivist mid-clone can take up to five minutes.

**That slowness is accepted deliberately.**  The alternative —
shortening the grace so updates land faster — would sever exactly the
operations the drain exists to protect.  Slow-but-complete beats
fast-but-severed, and the operator retains the re-pull-and-redeploy
hammer for anything urgent.

## Deploying it

Its own Portainer stack, compose path `docker/watchtower.yaml`:

| Var | Value |
|---|---|
| `WATCHTOWER_IMAGE` | a pinned release, e.g. `containrrr/watchtower:1.7.1` |
| `WATCHTOWER_POLL` | seconds between checks (default 3600) |
| `TZ` | log timestamps |

Verify it sees only what it should:

```sh
docker logs watchtower | head -20     # names the containers it is watching
```

The list should be the labelled workers and nothing else — in
particular **not** the agent.
