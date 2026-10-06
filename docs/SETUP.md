# Workload setup

[Documentation](README.md) | [Repository](../README.md)

Open **Manage workloads** from GPU Control in GNOME's top-right Quick Settings
menu. The packaged setup application is also available in the application list.

1. Choose ComfyUI, Ollama, llama.cpp or vLLM and select **Add workload**.
2. Choose a detected instance. **Refresh discovery** reads its existing inventory;
   it never starts an application or loads a model.
3. Choose a native model, or an explicit model file/folder. ComfyUI skips model
   selection because workflows choose models. File selection does not establish
   compatibility.
4. **Save drafts** to finish later. Drafts retain application, address/path and model choices, and Advanced launch binding fields. Saved bindings remain unverified; fingerprints are recomputed during verification. Drafts are separate from selectable workloads.
5. To configure an existing isolated service, open **Advanced launch binding**.
   Supply its service, cgroup and health URL. Native model bindings also require
   an instance ID, exact model ID, base URL and loaded service file. Setup reads
   the file fingerprint and verifies the binding without changing the service.
6. Select **Verify binding and add for review**, then **Review configuration**.
   Switch to Idle and finish active jobs before confirming **Apply configuration**.

Each model needs a distinct existing launch unit. Shared-unit model presets are
not supported. Setup does not create or rewrite launch services. Model identity
is checked again before GPU admission when switching workloads.

Configured workloads can be renamed or edited. **Remove from supervisor** changes
only supervisor configuration. Active/referenced workloads cannot be removed;
finish their work and switch to Idle first. Applications, models, workflows and
external configuration are preserved.

**Set up later** closes the window without applying configuration. Save drafts
first if you want to keep new choices. A stale-draft error requires reopening
Manage workloads before retrying. Interrupted activation resumes its recorded
configuration, with editing disabled until it completes.

Native GTK keyboard/accessibility checks and real GPU qualification remain part
of the GNOME/package host acceptance tracked in
[issue #146](https://github.com/mickey-kras/gpu-workload-supervisor/issues/146)
and [issue #148](https://github.com/mickey-kras/gpu-workload-supervisor/issues/148).
Automated widget and protocol tests do not qualify the host installation.

## Application discovery

`gpu-setup discover` returns saved setup state plus `applications` candidates for
ComfyUI, Ollama, llama.cpp and vLLM. It probes four default loopback ports and
inspects recognizable user-service launch metadata. A service candidate and an
endpoint candidate are separate observations; discovery does not correlate them
or verify lifecycle ownership. Existing catalog profiles remain unchanged.

For a non-default endpoint or explicit reference:

```sh
printf '%s' '{"app":"ollama","endpoint":"http://127.0.0.1:11434"}' | gpu-setup probe
printf '%s' '{"app":"llama.cpp","reference":"/models/model.gguf","referenceKind":"model-file"}' | gpu-setup probe
```

`app` accepts `comfyui`, `ollama`, `llama.cpp`, `vllm`. Select exactly one
`endpoint` or `reference`. `referenceKind` accepts `application`, `configuration`,
`model-file`, `model-directory`. Endpoint values are loopback HTTP(S) origins,
without credentials, path, query or fragment. References must be absolute clean
paths; only regular files and directories are accepted. File contents are not
executed or inspected, and directories are not enumerated.

| Application | Read-only observations |
| --- | --- |
| ComfyUI | `/system_stats` version; workflows select models, no model picker |
| Ollama | `/api/version`, available `/api/tags`, loaded `/api/ps`; remote entries marked `non-local` |
| llama.cpp | Native `/models` with explicit statuses, or older `/v1/models` served identities; simple launch model references |
| vLLM | `/version` and `/v1/models`; same-root aliases grouped; simple launch model references |

Missing optional APIs report `unsupported` or unknown loaded state. Unreachable
instances never become successful empty inventories. A verified stopped unit is
`not-running`; a failed endpoint connection is `unreachable`. Saved references
survive either condition. Served identities do not establish a disk inventory or
distinct physical models. File selection does not establish compatibility.

All candidates have `lifecycleControl: "unverified"`. They cannot activate a
workload by themselves. Application management and model switching require the
separate validated catalog/runtime path.

Discovery uses fixed GET routes, disables redirects and ambient proxies, limits
responses to 1 MiB, and bounds each probe to 3 seconds and discovery to 15 seconds.
It never uses llama.cpp reload/routed autoload requests, starts applications,
loads/unloads models, downloads files, scans drives, or writes application or
supervisor configuration. Configuration files with custom syntax, shell wrappers
and escaped launch commands remain explicit manual references.
