# Runtime refactor scope

This branch is a preparation step. It does not complete the daemon migration.

| Target | Current implementation | Remaining work |
| --- | --- | --- |
| One state writer | Existing locked SQLite clients | One daemon, a Unix socket API, and a stateless CLI |
| One state machine per GPU | Optional UUID for runtime probes and owned launches | Device-keyed state, leases, transitions, and migration |
| Workload drivers | Optional sleep and wake methods on the existing runtime | Separate service and transient-job drivers |
| Admission proxy | Existing request accounting and fences | Move state writes and lifecycle requests to the daemon |

This branch removes the experimental training executables and their framework dependencies.
The generic finite-job replacement remains in [issue 451](https://github.com/mickey-kras/gpu-workload-supervisor/issues/451).
The existing finite-job records and lifecycle stay readable for recovery.
The older training-runner documents describe the removed experimental implementation.

Existing template validation, template approval, resource limits, and setup cleanup stay supported.
Their replacements must preserve the accepted configuration and desktop actions.
Removing their implementations alone would break those contracts.

## Sleep and wake

Sleep is off by default. A profile can request it with `serving.sleepMode`.
A level 1 sleep offloads weights to RAM and discards the KV cache.
It does not let an active model exceed the GPU's VRAM capacity.

The supervisor verifies the launch binding, service state, cgroup, and GPU process memory.
A sleeping workload must report zero allocated VRAM for its compute processes.
Unknown memory or process ownership is not release evidence.
A failed sleep or release check falls back to a service stop.
A wake uses the normal release, external-process, capacity, and limit checks.
A wake response must also confirm that the engine is awake.

Sleep-enabled owned units omit mutual systemd conflicts.
Otherwise, starting the next unit would stop the sleeping process and discard its RAM cache.
The supervisor still checks GPU release before it starts the next workload.

The HTTP endpoints for sleep in vLLM need `VLLM_SERVER_DEV_MODE=1`.
The [vLLM security guidance](https://docs.vllm.ai/en/latest/usage/security/) describes the unauthenticated development endpoints.
This branch does not enable that mode or relax its rejection.
A stock qualified server therefore uses the cold-stop fallback when sleep endpoints are unavailable.
Protected lifecycle control and real-GPU qualification remain prerequisites for usable hibernation.
