# Runtime refactor scope

This branch is a preparation step. It does not complete the daemon migration.

| Target | Current implementation | Remaining work |
| --- | --- | --- |
| One state writer | Existing locked SQLite clients | One daemon, a Unix socket API, and a stateless CLI |
| One state machine per GPU | Optional UUID for runtime probes and owned launches | Device-keyed state, leases, transitions, and migration |
| Workload drivers | Cold start and stop on the existing runtime | Separate service and transient-job drivers |
| Admission proxy | Existing request accounting and fences | Move state writes and lifecycle requests to the daemon |

The packages keep the experimental training executables for existing installations.
An accepted trainer binding pins the executable path and SHA-256 digest for recovery and launch checks.
Retire the executables after the generic finite-job replacement provides a migration for these bindings.
See [issue 451](https://github.com/mickey-kras/llm-workload-supervisor/issues/451).

Existing template validation, resource limits, and setup cleanup stay supported.
Their replacements must preserve the accepted configuration and desktop actions.
Removing their implementations alone would break those contracts.

## Idle hibernation

The supervisor releases a vLLM workload with a cold stop.
A stop releases the unit, its cgroup and its GPU memory.
The next start uses the normal release, external-process, capacity and limit checks.

vLLM sleep mode needs `VLLM_SERVER_DEV_MODE=1` for its HTTP endpoints.
The [vLLM security guidance](https://docs.vllm.ai/en/latest/usage/security/) describes these unauthenticated development endpoints.
The supervisor refuses that mode, so it does not use vLLM sleep.
Reconsider sleep when vLLM offers it without development mode.
See [issue 629](https://github.com/mickey-kras/llm-workload-supervisor/issues/629).
