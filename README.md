# GPU Workload Supervisor

The supervisor switches one GPU between configured workloads (ComfyUI, vLLM).
It stops and verifies the outgoing workload before starting the next, and
blocks requests when a transition fails.

| Executable | Purpose |
| --- | --- |
| `gpu-mode` | Controller |
| `gpu-workload-proxy` | Admission proxy |
| `gpu-operator` | Local operator backend |
| `gpu-setup` | Detection and setup |
| `gpu-training-runner` | Experimental training runner |
| `gpu-training-candidate` | Preparation of unqualified inputs. Does not execute training. |
| `gpu-training-import` | Preparation of unqualified PEFT imports. Does not activate serving. |

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/architecture/generated/overview-dark.svg">
  <img src="docs/architecture/generated/overview-light.svg" alt="Clients send requests through the execution proxy to workload services sharing one GPU. A separate controller switches workloads. Both use durable state for admission and fencing.">
</picture>

- Controller: ownership and transitions.
- Proxy: admission and forwarding. No content policy.
- Brokers and gateways: external. Not installed by setup.
- Model checks: planned in the supervisor (#585). The file allowlist and upstream identity checks exist, but no command runs them yet. See [model checks](docs/MODEL-CHECKS.md).

[Architecture](docs/ARCHITECTURE.md).

## Requires

Linux with user systemd and cgroup v2.

## Install

Get a release archive or .deb from [Releases](https://github.com/mickey-kras/gpu-workload-supervisor/releases),
or build with `go build -o bin/ ./cmd/...`.

Setup detects and adopts supported systemd user services without rewriting their
source files. For GPU correction, preview first: the explicit `apply` action
creates a drop-in for an eligible stopped service and does not start it.

[Desktop setup](docs/DESKTOP.md).

## Setup checks

A path passes when it exists, you or root own it, and it is not world-writable.
Setup permits group write only for groups you belong to. The single-admin host
policy trusts members of your own groups.

Setup checks symlinks and their traversed ancestors, and reports broken symlinks
by name. It preserves supported direct commands and their arguments, and still
checks endpoint, model, GPU identity, environment and effective loaded settings.
Shell commands, environment files and uninspected operational directives remain
unsupported. See [launch constraints](docs/WORKLOADS.md).

## Docs

- [Deploy and qualify a host](docs/DEPLOYMENT.md)
- [Switch workloads, recover](docs/OPERATIONS.md)
- [Backup and restore](docs/RESTORING.md)
- [All documentation](docs/README.md)

[MIT license](LICENSE).
