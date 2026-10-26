# Configured workloads

[Documentation](README.md) | [Repository](../README.md)

An enabled catalog is a supported-version JSON object containing 1-32 `profiles`:

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

Catalog versions advance per feature, rather than bundling unrelated additions into one
schema change:

| Version | Feature boundary | Status |
| --- | --- | --- |
| 1 | Existing workload profiles and native model bindings | Supported |
| 2 | Adds owned native launch specifications | Supported |
| 3 | Planned fields for llama.cpp router bindings | Not yet supported |
| 4 | Finite trainer bindings and fenced job lifecycle | Supported |
| Later | Later features receive separate version changes | Not yet supported |

Existing v1 and v2 catalogs remain valid under their current rules. Unchanged profiles
need no migration or version bump. Owned launch specifications need v2 or v4.

Finite trainer bindings need v4. See [Finite training jobs](TRAINING.md). The validator
rejects unsupported versions (including planned v3), unknown fields and fields outside
their supported version.

A planned version does not enable its feature. Catalog versions are separate from
database schema versions and upstream runtime and API versions.

Removing the final application through setup writes an explicit disabled catalog, such
as `{"version":1,"profiles":[],"disabled":true}`, with no configured workloads. Empty
catalogs without `disabled: true` and disabled catalogs containing profiles are invalid.
Adding an application clears `disabled` and restores the ordinary profile requirements.

IDs contain at most 64 lowercase ASCII letters, digits, underscores and hyphens,
starting with a letter. The catalog reserves `idle` and `unknown`. Labels contain at
most 80 characters and 128 UTF-8 bytes, contain visible non-whitespace text, and exclude
control characters and bidirectional override or isolation characters.

Unit names and cgroups must be unique. Cgroups must be absolute, clean, non-root and
non-overlapping. Endpoints must use HTTP(S) on loopback without credentials or
fragments.

The validator rejects unknown fields, duplicate JSON keys, trailing values and files
larger than 352 KiB. Canonical catalog data excluding derived owned conflicts remains
capped at 256 KiB. Only the valid owned peer graph can use the extra 96 KiB allowance.

The `systemd` adapter stops the unit and verifies its recursive cgroup is empty. The
compatibility `media-unload` adapter also requires `releaseURL`. HTTP unload success
never proves GPU release.

A live opposing unload runtime remains fail-closed until independently stopped. The
controller checks every configured opposing runtime before it starts a target.
`requiredMiB` optionally enables the existing NVIDIA capacity check, with trusted
`-nvidia-smi` and optional headroom flags.

`bootPolicy` defaults to `stop-to-idle`. Explicit `retain` preserves an already running
healthy workload during reconciliation. It never starts it.

Multiple running `retain` profiles fail closed. Healthy retained registrations keep their
completion tokens, fence and admission. To reproduce the legacy boot behavior, use
`retain` for Text and `stop-to-idle` for Media.

Accept configuration explicitly while holding the controller gate. Examples abbreviate
the [pinned absolute binary path](DEPLOYMENT.md#controller-configuration) as `gpu-mode`:

```sh
gpu-mode -state /absolute/state.db -catalog /absolute/workloads.json configure
```

The result contains an opaque `revision`. Later updates must supply it using
`-configuration-revision REVISION`. A successful update atomically persists the catalog,
history and revision and advances the state version.

The store rejects stale requests. It also rejects updates that remove or change a
profile referenced by active or desired state, unfinished work or unfinished
transitions. Initial adoption of live legacy state requires first reaching safe Idle
using existing controls.

Runtime commands use the durable snapshot, never silently re-read a catalog file:

```sh
gpu-mode -state /absolute/state.db -systemctl /usr/bin/systemctl reconcile
gpu-mode -state /absolute/state.db -systemctl /usr/bin/systemctl -target speech switch
gpu-mode -state /absolute/state.db -systemctl /usr/bin/systemctl status
```

Flags precede the command, following Go's flag parser. Select workloads only through
`switch -target`. Targets must be present in the effective catalog.

Proxies accept configured workload IDs and SQLite validates registration against the
accepted catalog and active allocation. Automation grants remain explicit: adding a
workload never grants an existing principal permission to select it.

Schema v11 rebuilds workload-constrained tables transactionally, retaining state,
fences, registrations, tokens, transitions, events and relationships. Older binaries
reject this schema. Rollback follows [RESTORING.md](RESTORING.md), using a compatible
verified backup and binary and configuration pair. Never edit schema metadata or
downgrade a live database.

## Native model bindings

A `systemd` profile can include `nativeModel` with `runtime` (`ollama`, `llama.cpp`, or
`vllm`), `instance`, exact API `model`, loopback base `endpoint`, absolute `launchFile`,
and its `launchSHA256`. Model names are case-sensitive. By default each workload needs
its own existing unit and non-overlapping cgroup.

Two models in the same runtime instance can use different units on the same endpoint.
The controller stops the old unit and verifies GPU release before it starts the next
unit. Re-selecting an active workload does not restart that unit.

Opposing units remain reconciled.

Ollama workloads can instead share one unit, cgroup, endpoint and instance with
different exact models. Untagged names canonicalize to `:latest`, so `m` and `m:latest`
are the same model and cannot bind two profiles. To start a sibling on the shared unit,
the controller evicts every other loaded model over the Ollama API.

It waits for eviction, preloads the target with infinite keep-alive, and verifies
`/api/ps`. The unit keeps running. Switching to Idle or a non-shared workload unloads
only the outgoing model and does not evict.

The release fails while any other model, such as an auto-loaded foreign model, stays in
`/api/ps`. While the unit runs, release evidence is the loaded-model list. Once the unit
is dead, the controller verifies recursive cgroup emptiness instead.

If the controller cannot check an unload, the switch fails. Absence from `/api/ps` is
scheduler bookkeeping. Driver VRAM recovery lags an unload by roughly 0.5–1.5s (Ollama
`waitForVRAMRecovery`).

VRAM can leak entirely in known cases ([Ollama issue
#10597](https://github.com/ollama/ollama/issues/10597)). Thus, cross-runtime admission
after a shared-unit unload relies on the `requiredMiB` capacity probe, not `/api/ps`
alone. llama.cpp and vLLM still need one unit per model.

The native HTTP tests replay version-tagged, sanitized source-derived `/api/ps` and
llama.cpp `/models` examples. See the provenance and synthetic mutations in [the fixture
notes](../internal/runtime/testdata/README.md). Native llama.cpp readiness continues to
query `/health` and `/v1/models`.

`/models` replay is test-only parser coverage. Parsing exact identity and status does
not prove router lifecycle safety, fencing, admission or GPU release, and adds no router
binding support.

Stop the workload proxies before applying a catalog change. Setup and CLI configure both
enforce the proxy lifetime lock, including in-flight requests.

Launch-file constraints:

- The launch file must match systemd's fragment with no pending daemon
  reload, and use the supported direct-command subset. Setup adopts supported drop-ins without changes and verifies them by hash. It still rejects unsupported or ambiguous drop-in configuration.
- The desktop user or root must own executables, scripts, launch sources, validated environment and data paths and their traversed directory ancestry. These items must not be world-writable. Setup allows group write only for groups the desktop user belongs to. This single-admin host policy trusts members of its own groups.
  These checks do not isolate workloads from group members who can change them.
  Setup resolves symlinks with ownership and permission checks on every traversed path. It rejects broken links and excessive link chains.
- Setup rejects shell wrappers, environment files, uninspected execution hooks, filesystem remapping directives and command substitution. Setup supports bounded direct `ExecStartPre` system checks and inspects them without running them.
- Setup does not change or install application or model files.
- llama.cpp requires an existing local model file. vLLM requires an existing
  local model directory. These model checks prove existence and type, not
  ownership, permissions or content safety.
- Ollama requires `serve`, explicit `OLLAMA_NO_CLOUD=1` and matching
  `OLLAMA_HOST`. The selected model must be local before preloading.

Adopted `Environment=` assignments support literal values, including quotes around a
whole assignment. HOME, cache, temporary-directory and token-path settings need existing
trusted paths. Setup never reads token contents.

`PATH`, `LD_LIBRARY_PATH` and `PYTHONPATH` need trusted absolute directories and reject
empty search entries. `LD_LIBRARY_PATH` also treats semicolons as separators. Nonempty
code-loading overrides such as `LD_PRELOAD`, `LD_AUDIT` and `PYTHONHOME` remain
unsupported.

Setup preserves other literal variables. It does not certify their application-specific
behavior. Unsupported expansion and environment files remain rejected.

Directory checks do not inspect ownership, permissions or contents of loaded modules or
libraries. The operator remains responsible for trusted local installation and
dependency contents. Setup preserves the source files and checks their effective loaded
assignments.

Literal and `%h` `WorkingDirectory=` settings use the same existing-directory trust
checks: setup checks symlink targets and every traversed ancestor for foreign ownership
and world write permissions. Their exact effective loaded paths must match inspection
during setup, start and temporary discovery. Cleanup can skip directory availability
after admission, while retaining source, command, loaded-property and invocation checks.

Existing services can keep `Wants=network-online.target`, `KillSignal=SIGINT`,
`NoNewPrivileges=true`, `PrivateTmp=true` or `false`, `ProtectHome=read-only`, and
`ProtectSystem=strict`. Setup and execution check their effective loaded properties
against the preserved sources. `Wants=` uses systemd's additive dependency behavior.

Empty assignments do not remove earlier dependencies. Other dependency targets remain
unsupported. These settings qualify compatible syntax and preserve isolation settings.

They do not prove the application can run within that sandbox. Setup preserves existing
`Type=` and `Restart=` values, including `Restart=on-failure`. Automatic restarts can
run outside controller admission.

Account for that in deployment.

ComfyUI direct commands can keep `--base-directory`, `--output-directory`,
`--temp-directory`, `--user-directory`, `--extra-model-paths-config` and
`--enable-manager`. Directory options need existing trusted directories. Each explicit
configuration file requires an existing trusted readable regular file.

Symlinks undergo the same path checks. Recognized endpoint and path options support
`--option=value` as well as separate values. Setup does not support endpoint-changing
TLS flags or abbreviations of qualified endpoint and path options.

The endpoint must use loopback HTTP. Setup rejects `--cuda-device`, `--default-device`
and their abbreviations because they override the inspected `CUDA_VISIBLE_DEVICES`
selection. Setup preserves multiple files and repeated configuration options (at most
256 arguments and 32 configuration files per option).

Cleanup skips availability checks for these paths while retaining the command syntax and
source checks. Setup retains other ComfyUI arguments but does not certify their
behavior. Configuration qualification checks the file's metadata, without parsing YAML
or certifying referenced model or custom-node content.

Manager qualification preserves the argument under the existing trusted
desktop-principal script model. Inspection never imports or runs plugins. It does not
certify manager downloads, scheduled installations, plugin behavior or model safety.

Protect management endpoints and direct runtime access as part of deployment
qualification. Exact original arguments and source fingerprints remain bound and checked
before execution. The upstream [ComfyUI command
definitions](https://github.com/Comfy-Org/ComfyUI/blob/master/comfy/cli_args.py) and
[startup handling](https://github.com/Comfy-Org/ComfyUI/blob/master/main.py) describe
these options.

The supervisor preserves their behavior.

Trusting library-search directories does not certify dynamic-loader resolution,
transitive dependencies or plugin behavior. Setup neither loads libraries nor removes
required settings to make checks pass.

`CUDA_VISIBLE_DEVICES` supports one full physical GPU UUID, recorded as `gpuUUID` in the
binding and checked against the configured NVIDIA GPU at preflight, startup, readiness
and release. Numeric indices, UUID prefixes, multiple devices and MIG mappings remain
unsupported. Setup never removes or rewrites the setting to make adoption pass.

Numeric CUDA visibility has an explicit unsupported disposition: a discovered
installation remains installed, but its GPU identity stays unresolved and setup blocks
adoption. NVIDIA inventory indices are not CUDA logical ordinals. One inventory device
at index zero and no `CUDA_DEVICE_ORDER` in filtered unit metadata are insufficient
evidence.

They do not prove application ordering, inherited environment, environment-file
overrides or CUDA-visible device mapping. Metadata-only setup has no authoritative
same-context CUDA enumeration evidence and never starts a runtime to infer that mapping.
Adding an ordering setting alone does not qualify the numeric selection.

To adopt this installation, review and approve a change to use the configured GPU's full
UUID in its external service. Reload the user systemd manager. Refresh the binding.

Setup preserves the original files and settings.

The [NVIDIA CUDA environment-variable
reference](https://docs.nvidia.com/cuda/cuda-programming-guide/05-appendices/environment-variables.html)
defines CUDA ordering and visibility. For management indices, see the [NVIDIA
reference](https://docs.nvidia.com/deploy/nvidia-smi/index.html). The [systemd execution
environment
reference](https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html)
describes other environment sources. Do not treat the unit's `Environment` property as a
complete process-environment snapshot.

Supervisor-managed native launches can specify an absolute `owned.executable` path. It
must identify the selected runtime and pass the same executable trust checks. Omitting
it retains the standard `/usr/bin` location. You must still select a model, review the
side-effect-free preview and explicitly confirm activation.

Supervisor-owned Ollama units set `OLLAMA_MAX_LOADED_MODELS=1`. This limits the loaded
model count, not memory used by parallel requests to the same model. Eviction,
admission, readiness and release checks still apply.

Readiness requires native health plus exactly the selected model identity. Ollama checks
its loaded model list. A native proxy accepts only supported inference routes with the
exact JSON `model`.

It rejects lifetime overrides, ambiguous keys, query parameters and encoded bodies. When
its catalog revision changes, it stops accepting new requests. Its upstream must match
the binding.

Supervisor-mode execution of a native binding finishes itself: the proxy observes the
upstream response and completes the registration on verified terminal evidence. Ollama
generation and chat end with a `"done":true` object. OpenAI-compatible chat and
completion streams end with a `data: [DONE]` event.

Non-streaming responses end when every choice carries a non-null `finish_reason`.
Embedding responses end when the complete result body parses.

EOF, disconnects, errors and ambiguous bodies never complete work. Unproven
registrations stay unfinished for explicit reconciliation, and an external completion
callback remains accepted first-come. Direct access to native API ports bypasses proxy
enforcement and must remain outside the managed execution path.

You must still qualify the real GPU, systemd and runtime on the deployment host. HTTP
and lifecycle fixtures are not hardware qualification.

## ComfyUI execution completion

A non-native configured workload can opt into the proxy's [qualified ComfyUI
asynchronous adapter](PROXY-CONTRACT.md#opt-in-comfyui-asynchronous-execution). This
adds durable enqueue and completion correlation without a catalog version change, launch
discovery, graph storage, scheduler or automatic idle setting. Existing systemd workload
lifecycle, readiness, drain and GPU release verification still apply. Qualify trusted
ingress, gateway graph validation and direct-runtime isolation separately on the
deployment host.
