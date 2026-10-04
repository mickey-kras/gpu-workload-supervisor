# GPU Workload Supervisor documentation

[Repository](../README.md)

Start with [deployment](DEPLOYMENT.md), [boot reconciliation](OPERATIONS.md#boot-and-explicit-recovery), and [proxy setup](EXECUTION-PROXY.md). Complete [host qualification](DEPLOYMENT.md#qualify-the-host) before enabling execution.

## Deploy and operate

- [GNOME desktop installation and setup](DESKTOP.md): package activation, maintenance and rollback
- [Deployment](DEPLOYMENT.md): runtime flags, cgroups, capacity checks and host qualification
- [Operations](OPERATIONS.md): boot, switching, ownership, unfinished work and recovery
- [Execution proxy](EXECUTION-PROXY.md): route configuration, admission and completion
- [Backup, restore and rollback](RESTORING.md)
- [Security policy](../SECURITY.md)

## Understand and integrate

- [State and responsibility](ARCHITECTURE.md)
- [Proxy adapter contract](PROXY-CONTRACT.md): runtime evidence required before enabling execution
- [External control contract](external-control.md): restricted automation API
- [Future client plan](CLIENTS.md): dashboard and desktop clients, not implemented

## Maintain

- [Development and tests](DEVELOPMENT.md)
- [Repository controls](REPOSITORY-CONTROLS.md), [branching](../.github/BRANCHING.md) and [dependency updates](DEPENDABOT.md)
- [Releasing](RELEASING.md) and [v0.1.5 operator notes](releases/v0.1.5.md)
- [License](../LICENSE)
