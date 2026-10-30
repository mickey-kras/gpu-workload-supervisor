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
| 3 | Reserved | Not supported |
| 4 | Finite trainer bindings and fenced job lifecycle | Supported |
| Later | Later features receive separate version changes | Not yet supported |

Existing v1 and v2 catalogs remain valid under their current rules. Unchanged profiles
need no migration or version bump. Owned launch specifications need v2 or v4.

Finite trainer bindings need v4. See [Finite training jobs](TRAINING.md). The validator
rejects unsupported versions (including v3), unknown fields and fields outside
their supported version.

The hardened vLLM fields `chatTemplate`, `chatTemplateSHA256` and
`environmentFile` are valid in v1, v2 and v4 catalogs. They do not change the
catalog version. Earlier releases reject them as unknown fields. To go back to
an earlier release, restore a verified backup as
[RESTORING.md](RESTORING.md) describes.

Catalog versions are separate from database schema versions and upstream runtime and
API versions.

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
compatibility `media-unload` adapter also needs `releaseURL`. HTTP unload success
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
workload never grants an existing principal permission to choose it.

Schema v11 rebuilds workload-constrained tables transactionally, retaining state,
fences, registrations, tokens, transitions, events and relationships. Older binaries
reject this schema. Rollback follows [RESTORING.md](RESTORING.md), using a compatible
verified backup and binary and configuration pair. Never edit schema metadata or
downgrade a live database.

## Native model bindings

A `systemd` profile can include `nativeModel` with `runtime` `vllm`, `instance`,
exact API `model`, loopback base `endpoint`, absolute `launchFile`, and its
`launchSHA256`. A vLLM binding also records `chatTemplate`, its
`chatTemplateSHA256` and the `environmentFile` path of the key file. Setup
fills these fields. A binding without them never qualifies. Model names are case-sensitive. Each workload needs its own
existing unit and non-overlapping cgroup.

Two models in the same runtime instance can use different units on the same endpoint.
The controller stops the old unit and verifies GPU release before it starts the next
unit. Re-selecting an active workload does not restart that unit. Opposing units
stay reconciled.

Native readiness queries `/health`, `/version` and `/v1/models`. The
`/v1/models` request sends the API key from the key file of the binding.

### Hardened vLLM launch

Every vLLM launch must use these settings. A launch without one of them does
not qualify, and the workload stays unavailable:

- `--host 127.0.0.1`. vLLM listens only on the loopback address.
- `--load-format safetensors`. vLLM loads only safetensors weights.
- `--chat-template <absolute path>`. Setup records the SHA-256 of the template
  file. A changed template file stops the qualification until you run setup
  again. A changed, missing or unreadable template file does not block the
  setup that pins the new template or removes the workload.
- `Environment=HF_HUB_OFFLINE=1`. vLLM loads the model only from local files.
- `EnvironmentFile=<absolute path>`, without the glob characters `*`, `?`, `[`
  and `]`. This private file holds the API key as one
  line, `VLLM_API_KEY=<key>`. The key has 32 to 256 letters, digits, `-` or
  `_`, without `=` padding. The path must name the file itself, not a
  symbolic link. The desktop user must
  own the file. Set the file mode to `0600`. Do not put the key
  on the command line or in an `Environment=` line.

The option allowlist keeps the other vLLM options off. Thus remote code,
request-supplied chat templates, prompt-embedding input, an API key on the
command line and runtime adapter loading stay off.

In `Environment=` lines, setup accepts only these `VLLM_` settings:
`VLLM_LOGGING_LEVEL`, `VLLM_CONFIGURE_LOGGING`, `VLLM_NO_USAGE_STATS`,
`VLLM_DO_NOT_TRACK`, `VLLM_WORKER_MULTIPROC_METHOD`, `VLLM_ATTENTION_BACKEND`
and `VLLM_ENGINE_ITERATION_TIMEOUT_S`. Setup rejects all other `VLLM_`
settings, for example `VLLM_SERVER_DEV_MODE`, `VLLM_PLUGINS`,
`VLLM_ALLOW_INSECURE_SERIALIZATION` and `VLLM_ALLOW_RUNTIME_LORA_UPDATING`.
Setup does not read the environment of the user systemd manager. Do not set
`VLLM_` settings there (`systemctl --user set-environment`), because each user
service gets them.

The minimum vLLM version is 0.14.1. A model whose `config.json` or
`hf_quant_config.json` names NVFP4 needs vLLM 0.27.0 or later. Other FP4
schemes, such as MXFP4, need only 0.14.1. Readiness reads `/version` and keeps
the workload unavailable on an older version or a pre-release of the minimum
version. Setup does not check the version, because vLLM does not run during
setup. Use a vLLM version that has no known critical or high security
advisory.

For a supervisor-managed launch, setup pins `chat_template.jinja` in the model
folder. When you apply, setup creates the private key file under
`~/.config/gpu-workload-supervisor/vllm/`. Preview and review never create it.
Setup keeps a usable key file and never replaces a file that it cannot use. For
an existing launch, setup reads the template path and the key file path from
the launch.

For an existing launch, you create the key file. This command creates a
usable file:

```sh
umask 077 && printf 'VLLM_API_KEY=%s\n' "$(head -c 32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=\n')" > "$HOME/.config/vllm-api-key.env"
```

When you remove a supervisor-managed workload, setup removes its key file.
Setup does not remove the key file of an existing launch. If setup stops
before it commits, a new key file can stay. The next apply for the workload
uses it. If the key file of a supervisor-managed workload is missing, the next
apply creates a new one.

The workload proxy reads the key file for each request and sends the key to
vLLM. It replaces any `Authorization` header from the client. Readiness and the
proxy send the key only when the desktop user owns each listening socket for
127.0.0.1 on the vLLM port. Thus a program of another user that holds the
port does not get the key. A small time remains between this check and the
connection.

When the key file is missing or unusable, or another user holds the port, the
proxy does not forward the request. The client gets `502 Bad Gateway`, and the
proxy log names the cause. Correct the key file or stop the other program. If
vLLM rejects the key, readiness reports that the key differs from the key of
the running vLLM. Restart the workload, or run setup again if the key file
changed.

The key protects only the `/v1` routes of vLLM. Other vLLM routes do not need
the key, for example `/health`, `/version`, `/invocations`, `/tokenize`,
`/detokenize`, `/pooling`, `/score`, `/classify` and `/v2/rerank`. Some of them
run the model. The loopback listener limits these routes to programs on this
computer, including programs of other users. Programs of the same desktop
user can also read the key file. The supervisor trusts that user.

Bindings from an earlier release do not record these settings, and they do
not qualify. The workload proxy of such a binding does not start until you
configure the binding again. The bindings do not block setup: you can remove
them or configure them again.

- For a supervisor-managed binding, add `chat_template.jinja` to the model
  folder. Open the setup application and configure the model again. Setup generates the
  hardened launch.
- For an existing launch, add the settings to its unit and reload systemd.
  Run setup again for the workload.

Stop the workload proxies before applying a catalog change. Setup and CLI configure both
enforce the proxy lifetime lock, including in-flight requests.

Launch-file constraints:

- The launch file must match systemd's fragment with no pending daemon
  reload, and use the supported direct-command subset. Setup adopts supported drop-ins without changes and verifies them by hash. It still rejects unsupported or ambiguous drop-in configuration.
- The desktop user or root must own executables, scripts, launch sources, validated environment and data paths and their traversed directory ancestry. These items must not be world-writable. Setup allows group write only for groups the desktop user belongs to. This single-admin host policy trusts members of its own groups.
  These checks do not isolate workloads from group members who can change them.
  Setup resolves symlinks with ownership and permission checks on every traversed path. It rejects broken links and excessive link chains.
- Setup rejects shell wrappers, environment files other than the vLLM key file, uninspected execution hooks, filesystem remapping directives and command substitution. Setup supports bounded direct `ExecStartPre` system checks and inspects them without running them.
- Setup does not change or install application or model files.
- The vLLM runtime needs an existing local model directory. These model checks prove existence and type, not
  ownership, permissions or content safety.

Adopted `Environment=` assignments support literal values, including quotes around a
whole assignment. HOME, cache, temporary-directory and token-path settings need existing
trusted paths. Setup never reads token contents.

`PATH`, `LD_LIBRARY_PATH` and `PYTHONPATH` need trusted absolute directories and reject
empty search entries. `LD_LIBRARY_PATH` additionally treats semicolons as separators. Nonempty
code-loading overrides such as `LD_PRELOAD`, `LD_AUDIT` and `PYTHONHOME` stay
unsupported.

Setup rejects `VLLM_API_KEY` in `Environment=`. Other `VLLM_` variables must
be on the allowlist in [Hardened vLLM launch](#hardened-vllm-launch).

Setup keeps other literal variables. It does not certify their application-specific
behavior. Unsupported expansion and other environment files remain rejected.

Directory checks do not inspect ownership, permissions or contents of loaded modules or
libraries. The operator stays responsible for trusted local installation and
dependency contents. Setup preserves the source files and checks their effective loaded
assignments.

Literal and `%h` `WorkingDirectory=` settings use the same existing-directory trust
checks. Setup checks symlink targets and every traversed ancestor for foreign ownership
and world write permissions. Their exact effective loaded paths must match inspection
during setup and start.

Existing services can keep `Wants=network-online.target`, `KillSignal=SIGINT`,
`NoNewPrivileges=true`, `PrivateTmp=true` or `false`, `ProtectHome=read-only`, and
`ProtectSystem=strict`. Setup and execution check their effective loaded properties
against the preserved sources. `Wants=` uses systemd's additive dependency behavior.

Empty assignments do not remove earlier dependencies. Other dependency targets remain
unsupported. These settings qualify compatible syntax and keep isolation settings.

They do not prove the application can run within that sandbox. Setup preserves existing
`Type=` and `Restart=` values, including `Restart=on-failure`. Automatic restarts can
run outside controller admission. Account for that in deployment.

ComfyUI direct commands can keep `--base-directory`, `--output-directory`,
`--temp-directory`, `--user-directory`, `--extra-model-paths-config` and
`--enable-manager`. Directory options need existing trusted directories. Each explicit
configuration file needs an existing trusted readable regular file.

Symlinks undergo the same path checks. Recognized endpoint and path options support
`--option=value` as well as separate values. Setup does not support endpoint-changing
TLS flags or abbreviations of qualified endpoint and path options.

The endpoint must use loopback HTTP. Setup rejects `--cuda-device`, `--default-device`
and their abbreviations because they override the inspected `CUDA_VISIBLE_DEVICES`
selection. Setup preserves multiple files and repeated configuration options (at most
256 arguments and 32 configuration files per option).

Setup retains other ComfyUI arguments but does not certify their
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
these options. The supervisor preserves their behavior.

Trusting library-search directories does not certify dynamic-loader resolution,
transitive dependencies or plugin behavior. Setup neither loads libraries nor removes
required settings to make checks pass.

`CUDA_VISIBLE_DEVICES` supports one full physical GPU UUID, recorded as `gpuUUID` in the
binding. The supervisor checks it against the configured NVIDIA GPU at preflight,
startup, readiness and release. Numeric indices, UUID prefixes, multiple devices and MIG mappings stay
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

Readiness requires native health plus exactly the chosen model identity. A native proxy accepts only supported inference routes with the
exact JSON `model`. It rejects lifetime overrides, ambiguous keys, query parameters and encoded bodies. When
its catalog revision changes, it stops accepting new requests. Its upstream must match
the binding.

Supervisor-mode execution of a native binding finishes itself: the proxy observes the
upstream response and completes the registration on verified terminal evidence.
OpenAI-compatible chat and completion streams end with a `data: [DONE]` event.

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
