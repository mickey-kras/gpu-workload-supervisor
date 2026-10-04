# Configured workloads

Existing Text/Media flags remain supported for deployments without an accepted
catalog. A catalog is a version 1 JSON object containing 1–32 `profiles`:

```json
{
  "version": 1,
  "profiles": [
    {
      "id": "speech",
      "label": "Speech",
      "adapter": "systemd",
      "unit": "speech.service",
      "cgroup": "/user.slice/user-1000.slice/user@1000.service/app.slice/speech.service",
      "healthURL": "http://127.0.0.1:9000/health",
      "requiredMiB": 4096,
      "bootPolicy": "stop-to-idle"
    }
  ]
}
```

IDs contain at most 64 lowercase ASCII letters, digits, underscores and hyphens,
starting with a letter. `idle` and `unknown` are reserved. Labels contain at most
80 characters and 128 UTF-8 bytes, contain visible non-whitespace text, and exclude
control characters and bidirectional override/isolation characters. Unit names and cgroups must be unique;
cgroups must be absolute, clean, non-root and non-overlapping. Endpoints must use
HTTP(S) on loopback without credentials or fragments. Unknown fields, duplicate
JSON keys, trailing values and files larger than 64 KiB are rejected.

The `systemd` adapter stops the unit and verifies its recursive cgroup is empty.
The compatibility `media-unload` adapter also requires `releaseURL`; HTTP unload
success never proves GPU release. A live opposing unload runtime remains
fail-closed until independently stopped. Every configured opposing runtime is
checked before a target starts. `requiredMiB` optionally enables the existing
NVIDIA capacity check, with trusted `-nvidia-smi` and optional headroom flags.

`bootPolicy` defaults to `stop-to-idle`. Explicit `retain` preserves an already
running healthy workload during reconciliation; it never starts it. Multiple
running retain profiles fail closed. Healthy retained registrations keep their
completion tokens, fence and admission. To reproduce legacy boot behavior use
`retain` for Text and `stop-to-idle` for Media.

Accept configuration explicitly while holding the controller gate:

```sh
gpu-mode -state /absolute/state.db -catalog /absolute/workloads.json configure
```

The result contains an opaque `revision`. Subsequent updates must supply it using
`-configuration-revision REVISION`. A successful update atomically persists the
catalog, history and revision and advances the state version. Stale requests or
updates removing/changing a profile referenced by active/desired state,
unfinished work or unfinished transitions are rejected. Initial adoption of live
legacy state requires first reaching safe Idle using existing controls.

Runtime commands use the durable snapshot, never silently re-read a catalog file:

```sh
gpu-mode -state /absolute/state.db -configured -systemctl /usr/bin/systemctl reconcile
gpu-mode -state /absolute/state.db -configured -systemctl /usr/bin/systemctl -workload speech switch
gpu-mode -state /absolute/state.db -configured -systemctl /usr/bin/systemctl status
```

Flags precede the command, following Go's flag parser. Legacy per-workload flags
conflict with catalog configuration. Existing `text`, `media`, `idle` and ownership
commands remain available; targets must be present in the effective catalog.
Proxies accept configured workload IDs and SQLite validates registration against
the accepted catalog and active allocation. Automation grants remain explicit:
adding a workload never grants an existing principal permission to select it.

Schema v11 rebuilds workload-constrained tables transactionally, retaining state,
fences, registrations, tokens, transitions, events and relationships. Older binaries
reject this schema. Rollback follows [RESTORING.md](../RESTORING.md), using a
compatible verified backup and binary/configuration pair; never edit schema
metadata or downgrade a live database.
