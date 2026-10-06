# GPU Workload Supervisor

[![PR validation](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/pr-validation.yml/badge.svg?event=pull_request)](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/pr-validation.yml?query=event%3Apull_request)
[![coverage](https://img.shields.io/badge/coverage-%E2%89%A590%25%20%28CI--gated%29-brightgreen)](https://github.com/mickey-kras/gpu-workload-supervisor/blob/main/.github/workflows/ci.yml)
[![codeql](https://img.shields.io/github/check-runs/mickey-kras/gpu-workload-supervisor/main?nameFilter=codeql%20%2F%20analyze&label=codeql&logo=github)](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/main.yml?query=branch%3Amain)
[![aislop](https://badges.scanaislop.com/score/mickey-kras/gpu-workload-supervisor.svg)](https://scanaislop.com/mickey-kras/gpu-workload-supervisor)
[![main + SonarQube](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/main.yml/badge.svg?branch=main)](https://github.com/mickey-kras/gpu-workload-supervisor/actions/workflows/main.yml?query=branch%3Amain)
[![license: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

GPU Workload Supervisor switches between text and media workloads that share one GPU. It stops and verifies the outgoing workload before starting the next, and blocks new requests when a transition or recovery fails. Use it when both workloads cannot safely run at once.

It provides four executables: `gpu-mode` (controller and workload transitions), `gpu-workload-proxy` (execution admission proxy), `gpu-operator` (one-request local operator backend), and `gpu-setup` (guided setup and catalog commit). You supply the runtime services, health endpoints and deployment configuration.

Use this project independently of the memory stack. It does not require Memory Router, its agent integrations, or Hindsight.

## Architecture

<a href="docs/ARCHITECTURE.md">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/architecture/generated/overview-dark.svg">
    <source media="(prefers-color-scheme: light)" srcset="docs/architecture/generated/overview-light.svg">
    <img alt="Clients reach text or media runtimes through the execution proxy. A separate controller switches workloads; both share durable state. Supervisor components are highlighted." src="docs/architecture/generated/overview-light.svg">
  </picture>
</a>

[Explore state and responsibilities](docs/ARCHITECTURE.md) | [Execution proxy details](docs/EXECUTION-PROXY.md)

## Install

Download the Linux amd64 or arm64 archive from [Releases](https://github.com/mickey-kras/gpu-workload-supervisor/releases), verify it against the release checksum file, and extract the four executables (`gpu-mode`, `gpu-workload-proxy`, `gpu-operator`, `gpu-setup`) into a version-specific directory. A Debian package with the same executables plus the GNOME desktop integration is published per architecture. Use absolute binary paths in commands and services. See [release verification](docs/RELEASING.md) and [backup before upgrading](docs/RESTORING.md#back-up-before-an-upgrade).

Requires Linux with user-systemd and cgroup v2. Direct runtime requests bypass the supervisor, so deployment must prevent that bypass.

## Set up your workloads

Follow [deployment setup](docs/DEPLOYMENT.md) to configure the two runtime services, a private durable state directory and health checks. The guide includes the full controller command and release-verification requirements.

Then [reconcile at boot](docs/OPERATIONS.md#boot-and-explicit-recovery), configure the [execution proxy](docs/EXECUTION-PROXY.md), and complete [host qualification](docs/DEPLOYMENT.md#qualify-the-host) before enabling requests. New state starts closed; failed transitions require explicit operator recovery.

## Documentation

- [Deploy and qualify a host](docs/DEPLOYMENT.md)
- [Switch workloads, transfer ownership and recover](docs/OPERATIONS.md)
- [Back up and restore](docs/RESTORING.md)
- [All documentation](docs/README.md), including development, contracts and releases

[MIT license](LICENSE).

