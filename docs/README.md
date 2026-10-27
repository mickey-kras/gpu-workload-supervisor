<a id="gpu-workload-supervisor-documentation"></a>

# Documentation for GPU Workload Supervisor

[Repository](../README.md)

Start with [deployment](DEPLOYMENT.md), [boot reconciliation](OPERATIONS.md#boot-and-explicit-recovery), and [proxy setup](EXECUTION-PROXY.md). Complete [host qualification](DEPLOYMENT.md#qualify-the-host) before you enable execution.

<a id="deploy-and-operate"></a>

## Deploy and use

- [GNOME desktop installation and setup](DESKTOP.md): setup application, application discovery, package activation, maintenance and rollback
- [Deployment](DEPLOYMENT.md): runtime flags, cgroups, capacity checks and host qualification
- [Operations](OPERATIONS.md): boot, switching, ownership, unfinished work and recovery
- [Optional requester ingress](REQUESTER-INGRESS.md): authenticated workflow authorization, durable demand and nonpreemptive activation. Auto-idle remains Off
- [Execution proxy](EXECUTION-PROXY.md): route configuration, admission and completion
- [Configured workloads](WORKLOADS.md): catalog profiles, adapters and schema migration
- [Finite training jobs](TRAINING.md): trainer binding, lifecycle evidence and recovery
- [Local operator protocol](OPERATOR.md): on-demand `/usr/bin/gpu-operator` request process and shipped GNOME extension contract
- [Backup, restore and rollback](RESTORING.md)
- [Security policy](../SECURITY.md)

## Understand and integrate

- [Architecture](ARCHITECTURE.md)
- [Host-protection resource limits](RESOURCE-LIMITS.md): design for an opt-in host cap and limits on owned units. Not implemented
- [Proxy adapter contract](PROXY-CONTRACT.md): runtime evidence required before you enable execution
- [Demand activation and inactivity idle](AUTO-IDLE.md): durable policy, settings, activation and a 60-second timer. The policy defaults to Off. A qualified evidence provider is necessary to enable it

<a id="maintain"></a>

## Maintenance

- [Development and tests](DEVELOPMENT.md)
- [Releasing](RELEASING.md) and [v0.1.5 operator notes](releases/v0.1.5.md)
- [License](../LICENSE)

