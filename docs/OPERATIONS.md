# Operate and recover workloads

[Documentation](README.md) | [Repository](../README.md)

Use the pinned binaries and complete [deployment configuration](DEPLOYMENT.md) for every command.

## Boot and explicit recovery

Configure user-systemd startup declaratively in the deployment repository:

1. Disable independent startup of both workload runtimes, including timers,
   sockets, update jobs, and other activation paths that could start them outside
   controller transitions. Retain the installed units for controller use.
2. Make a reconciliation step use the pinned CLI and complete runtime flags.
   Order it after its durable state and user-systemd prerequisites are available.
3. Require successful reconciliation before starting execution proxies or any
   automation that can request workloads. Ordering alone (`After=`) is not a
   success dependency; use a required dependency and propagate the reconciliation
   command's nonzero exit. Never prefix that command with `-` to ignore failure.
4. Keep failure latched and admission unavailable. Do not automatically invoke
   `recover`, `recover-user`, `resolve-work`, or `restore-state` from a restart
   loop. Check the recorded state and runtime evidence before explicit recovery.

Reconciliation does not resume interrupted jobs or take ownership from a user.
User-owned state requires operator inspection and `recover-user` verification;
it must not be silently converted to supervisor ownership during boot. After
successful explicit recovery, resume only the path appropriate to the verified
owner. A user-owned deployment must not repeatedly run supervisor reconciliation;
return control explicitly before resuming supervisor automation.
Actual unit definitions, identities, startup targets, and deployment automation
are outside this repository.

```sh
/PINNED/RELEASE/gpu-mode [runtime flags] status
/PINNED/RELEASE/gpu-mode [runtime flags] reconcile
/PINNED/RELEASE/gpu-mode [runtime flags] recover
```

`status` reports persisted ownership and admission plus observed active workload;
it is not a full readiness/release check and can latch an observation failure for
supervisor-owned state. Check the exit code as well as JSON. A successful idle
reconciliation is stable and healthy with admission closed. Select `text` or
`media` through the controller to admit that workload; do not start its runtime
independently. Use `recover` only after repairing the cause of a supervisor-owned
failure. User-owned recovery is described below.

## Ownership and unfinished work

Ownership changes always require a target. Add the normal runtime flags before the command:

```sh
/PINNED/RELEASE/gpu-mode [runtime flags] -target text take-control
/PINNED/RELEASE/gpu-mode [runtime flags] -target media user-switch
/PINNED/RELEASE/gpu-mode [runtime flags] -target idle return-control
/PINNED/RELEASE/gpu-mode [runtime flags] -target idle recover-user
```

- `take-control` changes supervisor ownership to user ownership. It closes admission, rotates the fence, drains registered work, and verifies the target before committing ownership.
- `user-switch` selects `text`, `media`, or `idle` while user-owned. `return-control` selects the supervisor's workload explicitly. Both authorize terminating all current user work, including queued media work and same-target transfers. They stop both runtime units before starting the selected target.
- `text`, `media`, and `idle` remain supervisor-only commands. They reject user-owned state.
- User ownership keeps supervisor admission closed. Healthy, stable user execution bypasses lease registration only for the selected workload. Execution is blocked while switching, idle, or in an error state.

User execution requests hold a shared cross-process handoff lock. User switches and returns close the gate, stop user runtimes, and wait up to `-drain-timeout` for forwarding handlers to exit before restarting anything. A stalled handler makes the operation fail closed; cancel the client request or stop the proxy before recovery. All proxies sharing a state database must use this version's handoff locking before enabling ownership commands.

Failed and interrupted operations retain the last committed owner and require explicit recovery. Stopped user work is never restarted by rollback. For supervisor ownership, use `recover`. For user ownership, inspect the runtime and run `/PINNED/RELEASE/gpu-mode [runtime flags] -target text|media|idle recover-user` with the workload you intend to keep. This command verifies the target without starting or stopping runtimes, preserves user ownership, and keeps supervisor admission closed. An idle recovery verifies release using the configured policy. If verification fails, repair or stop runtimes explicitly and retry. `reconcile` never automatically takes control from the user. Every transfer, user switch, and recovery rotates the fence and records durable source/target ownership and transition outcomes.

## Resolve orphaned work

If admitted work cannot report completion (for example, forwarding failed before either party received its registration token), stop every `gpu-workload-proxy` instance for this state file and wait for shutdown. Run `/PINNED/RELEASE/gpu-mode` with the complete runtime flags plus `-resolve-reason 'operator incident reference' resolve-work`. Proxy instances hold a shared lifetime lock; `resolve-work` refuses to run until they are gone, then closes admission, rotates the fence, stops both runtimes, verifies stopped units and empty workload cgroups, and atomically marks unfinished work abandoned with an audit record. Do not use an older proxy binary without the lifetime lock during this operation. If any stop or verification fails, work remains unfinished and admission stays closed. Run `recover` after successful resolution, then restart proxies and switch workload as needed. This operation terminates all running work, so use it only after investigating the orphaned requests.

## Restore state

Restored databases require an explicit `/PINNED/RELEASE/gpu-mode -state /PRIVATE/DURABLE/STATE/state.db restore-state` before any proxy starts. This command needs no runtime flags. It does not copy a backup; follow the [restore procedure](RESTORING.md) for ordering and validation. Normal restart does not rotate the fence.
