# Documentation for GPU Workload Supervisor

[Repository](../README.md)

Start with desktop setup or host deployment. Complete host and adapter qualification before you enable execution.

## Deploy and use

| Task | Guide |
| --- | --- |
| Install the desktop tools | [Desktop setup](OPERATIONS.md#local-gnome-desktop-installation) |
| Configure and qualify a host | [Deployment](DEPLOYMENT.md) |
| Switch workloads or recover | [Operation and recovery](OPERATIONS.md#operate-and-recover-workloads) |
| Upgrade, restore or roll back | [Backup and restore](OPERATIONS.md#make-a-backup-restore-and-roll-back-state) |
| Prepare and qualify training | [Training](TRAINING.md) |

## Understand and integrate

- [Architecture and ownership](INTEGRATION.md#state-and-responsibility)
- [Workload configuration](INTEGRATION.md#configured-workloads)
- [Proxy setup](INTEGRATION.md#configure-the-execution-proxy) and [adapter contracts](INTEGRATION.md#contract-for-execution-proxy-adapters)
- [Local operator protocol](OPERATIONS.md#local-operator-protocol) and [requester ingress](OPERATIONS.md#optional-requester-ingress)
- [Optional demand activation and inactivity policy](OPERATIONS.md#demand-activation-and-inactivity-idle-plan): defaults to Off and requires a qualified evidence provider
- [Resource-limit design](RESOURCE-LIMITS.md): catalog v5 accepts the schema. Units with limits do not start yet

## Maintenance

- [Development and tests](MAINTENANCE.md#development)
- [Releases and publication recovery](MAINTENANCE.md#source-and-binary-releases)
- [CI policy](MAINTENANCE.md#ci-policy-changes) and [build-tool updates](MAINTENANCE.md#build-tool-updates)
- [Security policy](../SECURITY.md)
- [License](../LICENSE)
