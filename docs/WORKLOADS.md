# Configured workloads

A catalog is a version 1 JSON object containing 1-32 `profiles`:

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
gpu-mode -state /absolute/state.db -systemctl /usr/bin/systemctl reconcile
gpu-mode -state /absolute/state.db -systemctl /usr/bin/systemctl -workload speech switch
gpu-mode -state /absolute/state.db -systemctl /usr/bin/systemctl status
```

Flags precede the command, following Go's flag parser. Existing `text`, `media`,
`idle` and ownership
commands remain available; targets must be present in the effective catalog.
Proxies accept configured workload IDs and SQLite validates registration against
the accepted catalog and active allocation. Automation grants remain explicit:
adding a workload never grants an existing principal permission to select it.

Schema v11 rebuilds workload-constrained tables transactionally, retaining state,
fences, registrations, tokens, transitions, events and relationships. Older binaries
reject this schema. Rollback follows [RESTORING.md](RESTORING.md), using a
compatible verified backup and binary/configuration pair; never edit schema
metadata or downgrade a live database.

## Native model bindings

A `systemd` profile may include `nativeModel` with `runtime` (`ollama`,
`llama.cpp`, or `vllm`), `instance`, exact API `model`, loopback base `endpoint`,
absolute `launchFile`, and its `launchSHA256`. Model names are case-sensitive.
Each workload needs its own existing unit and non-overlapping cgroup. Two models
in the same runtime instance can use different units on the same endpoint; the
old unit stops and GPU release is verified before the next starts. Re-selecting
an active workload does not restart that unit; opposing units remain reconciled.
Shared-unit model replacement is unsupported. Stop workload proxies before
applying a catalog change; setup and CLI configure both enforce the proxy
lifetime lock, including in-flight requests.

Launch-file constraints:

- The launch file must match systemd's fragment, have no drop-ins or pending
  daemon reload, and use the supported direct-command subset.
- Executables and their resolved directory ancestry must be root-owned and not
  group/world writable. User-owned runtime installations are therefore not
  qualified by this adapter.
- Shell wrappers, hooks, environment files, downloads, automatic restart,
  command substitution, and unrecognized options are rejected.
- No application/model files are rewritten or installed.
- llama.cpp requires an existing local model file; vLLM requires an existing
  local model directory.
- Ollama requires `serve`, explicit `OLLAMA_NO_CLOUD=1` and matching
  `OLLAMA_HOST`; the selected model must be local before preloading.

Readiness requires native health plus exactly the selected model identity;
Ollama checks its loaded model list. A native proxy accepts only supported
inference routes with the exact JSON `model`, rejects lifetime overrides,
ambiguous keys, query parameters and encoded bodies, and stops accepting new
requests when its catalog revision changes. Its upstream must match the binding.

These bindings support operator switching. They do not add runtime-specific
terminal completion callbacks: Supervisor-mode execution still requires the
existing trusted completion adapter contract. Direct access to native API ports
bypasses proxy enforcement and must remain outside the managed execution path.
Real GPU/systemd/runtime qualification is still required on the deployment host;
HTTP and lifecycle fixtures are not hardware qualification.
