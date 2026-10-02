# Operate and recover workloads

[Documentation](README.md) | [Repository](../README.md)

Use the pinned binaries and complete [deployment configuration](DEPLOYMENT.md) for every command.

## Switch workloads

After successful reconciliation, run one command for the target you want:

```sh
/PINNED/RELEASE/gpu-mode [runtime flags] text
/PINNED/RELEASE/gpu-mode [runtime flags] media
/PINNED/RELEASE/gpu-mode [runtime flags] idle
```

Replace the binary path and `[runtime flags]` with your deployment configuration. Put flags before the single command. `text` and `media` admit the selected workload after verification; `idle` keeps admission closed. These commands require supervisor ownership. Do not start runtimes independently.

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
failure. Recovery first closes admission and rotates its fence. Before stopping
media it waits up to `-drain-timeout` for already admitted work to finish; old
registrations retain their completion authority. Failed or canceled recovery
stays closed with error health and unknown runtime state. Unfinished orphan work
requires the explicit `resolve-work` procedure below before destructive recovery
can proceed. User-owned recovery is described below.

Runtime error messages omit untrusted command output and may omit the underlying
probe detail. Inspect the affected unit with `systemctl --user status UNIT` and
`journalctl --user -u UNIT` for service diagnostics. Suppressed probe output is
not automatically copied to the journal.

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

User execution requests hold a shared cross-process handoff lock. User switches and returns close admission, stop user runtimes and verify release, then take the exclusive handoff lock. They wait up to `-drain-timeout` for forwarding handlers to exit before restarting anything. A stalled handler makes the operation fail closed; cancel the client request or stop the proxy before recovery. All proxies sharing a state database must use this version's handoff locking before enabling ownership commands.

### Recover after a failed transfer or switch

1. Inspect the recorded owner and runtime evidence. Failed or interrupted operations retain the last committed owner. Rollback never restarts stopped user work.
2. Repair the cause. For supervisor ownership, run `recover` with the complete runtime flags.
3. For user ownership, choose the workload to keep and verify it. For example:

   ```sh
   /PINNED/RELEASE/gpu-mode [runtime flags] -target media recover-user
   ```

   Use `text` or `idle` instead when appropriate. This verifies without starting or stopping runtimes, preserves user ownership and keeps supervisor admission closed. Idle recovery verifies release under the configured policy. If verification fails, repair or explicitly stop runtimes and retry.
4. Resume only the path appropriate to the verified owner. Do not repeatedly reconcile user-owned state; `reconcile` never takes it over.

Every transfer, user switch and recovery rotates the fence and durably records source/target ownership and the outcome.

## Resolve orphaned work

Use this only after investigating work that cannot report completion, such as a submission whose registration token was lost. It terminates all running work.

1. Stop every `gpu-workload-proxy` for the state file and wait for shutdown. Do not use older proxies without the lifetime lock.
2. Record an incident reason and resolve the work:

   ```sh
   /PINNED/RELEASE/gpu-mode [runtime flags] -resolve-reason 'operator incident reference' resolve-work
   ```

3. Confirm success. The command requires all proxy lifetime locks to be released, closes admission, rotates the fence, stops both runtimes and verifies stopped units and empty workload cgroups. It then atomically marks unfinished work abandoned and records an audit event. Any stop or verification failure leaves work unfinished and admission closed.
4. After successful resolution, run `recover`, restart proxies and select the needed workload.

## Restore state

Restored databases require an explicit `/PINNED/RELEASE/gpu-mode -state /PRIVATE/DURABLE/STATE/state.db restore-state` before any proxy starts. This command needs no runtime flags. It does not copy a backup; follow the [restore procedure](RESTORING.md) for ordering and validation. Normal restart does not rotate the fence.

## Retain or archive audit history

Audit deletion is explicit; ordinary proxy work retention does not remove audit
history. Set your retention period, create a consistent SQLite backup using the
[backup procedure](RESTORING.md#back-up-before-an-upgrade), and retain that archive
according to your audit requirements. Then run bounded batches:

```sh
/PINNED/RELEASE/gpu-mode -state /PRIVATE/DURABLE/STATE/state.db \
  -audit-before CUTOFF_RFC3339 -audit-batch 256 prune-audit
```

The cutoff and paths are placeholders. Repeat until `prunedAuditRecords` is zero.
A batch removes at most 256 audit parents (maximum configurable 1024), together
with their events and snapshots. In-progress and current-fence transitions and
transitions referencing unfinished work remain. Restoration and work-resolution
history is pruned only with no unfinished work or live transition, always retaining
the latest record of each kind. Resolve safety evidence through normal recovery;
do not delete protected records manually to meet a storage target. Archive more
often or provision additional storage if unresolved evidence keeps growing.
Current-fence transitions stay pinned until a normal switch, ownership change or
recovery rotates the fence. If the workload runs unchanged for a long time, plan
archive/storage checks on your retention schedule; do not rotate solely to prune.

Pruning holds the controller lock but does not require proxy shutdown: it changes
only eligible audit history, preserving unfinished-work evidence in one SQLite
transaction. Ctrl-C cancels a wait for the controller lock.

Deletion permits SQLite page reuse; it does not shrink the database file. Physical
reclamation is separate, planned offline maintenance: stop all proxies/controllers,
back up, then use SQLite `VACUUM` with adequate temporary disk space. Never replace
or copy a live database file or discard its WAL.
