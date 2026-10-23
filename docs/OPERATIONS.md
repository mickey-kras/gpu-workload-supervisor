<a id="operate-and-recover-workloads"></a>

# Use and recover workloads

[Documentation](README.md) | [Repository](../README.md)

Use the pinned binaries and complete [deployment configuration](DEPLOYMENT.md) for every command.

## Switch workloads

After successful reconciliation, select the target with one switch command:

```sh
/PINNED/RELEASE/gpu-mode [runtime flags] -target text switch
/PINNED/RELEASE/gpu-mode [runtime flags] -target media switch
/PINNED/RELEASE/gpu-mode [runtime flags] -target idle switch
```

Replace the binary path and `[runtime flags]` with your deployment configuration. Put flags before the single command. `text` and `media` admit the selected workload after verification. `idle` keeps admission closed. Switching requires supervisor ownership. Do not start runtimes independently.

## Boot and explicit recovery

Configure user-systemd startup declaratively in the deployment repository:

1. Disable independent startup of both workload runtimes. Include timers,
   sockets, update jobs, and other activation paths that can start them outside
   controller transitions. Keep the installed units for controller use.
2. Make a reconciliation step use the pinned CLI and complete runtime flags.
   Order it after its durable state and user-systemd prerequisites are available.
3. Make successful reconciliation mandatory before starting execution proxies or any
   automation that can request workloads. Ordering alone (`After=`) is not a
   success dependency. Use a required dependency and propagate the reconciliation
   command's nonzero exit. Never prefix that command with `-` to ignore failure.
4. Keep failure latched and admission unavailable. Do not automatically invoke
   `recover`, `recover-user`, `resolve-work`, or `restore-state` from a restart
   loop. Check the recorded state and runtime evidence before explicit recovery.

Reconciliation does not resume interrupted jobs or take ownership from a user.
User-owned state requires operator inspection and `recover-user` verification.
Boot automation must not silently convert it to supervisor ownership. After
successful explicit recovery, resume only the path appropriate to the verified
owner.

A user-owned deployment must not repeatedly run supervisor reconciliation.
Return control explicitly before resuming supervisor automation.
Actual unit definitions, identities, startup targets, and deployment automation
are outside this repository.

```sh
/PINNED/RELEASE/gpu-mode [runtime flags] status
/PINNED/RELEASE/gpu-mode [runtime flags] reconcile
/PINNED/RELEASE/gpu-mode [runtime flags] recover
```

- `status` reports persisted ownership and admission plus observed active
  workload. It is not a full readiness or release check and can latch an
  observation failure for supervisor-owned state. Check the exit code as well
  as JSON.
- A successful idle `reconcile` is stable and healthy with admission closed.
  Select `text` or `media` through the controller to admit that workload. Do
  not start its runtime independently.
- Use `recover` only after repairing the cause of a supervisor-owned failure.
  Recovery first closes admission and rotates its fence. Before stopping media
  it waits up to `-drain-timeout` for already admitted work to finish. Old
  registrations keep their completion authority. Failed or canceled recovery
  stays closed with error health and unknown runtime state.
- Unfinished orphan work requires the explicit `resolve-work` procedure below
  before destructive recovery can proceed.

Runtime error messages omit untrusted command output and can omit the underlying
probe detail. Inspect the affected unit with `systemctl --user status UNIT` and
`journalctl --user -u UNIT` for service diagnostics. The supervisor does
not automatically copy suppressed probe output to the journal.

## Ownership and unfinished work

Ownership changes always need a target. Add the normal runtime flags before the command:

```sh
/PINNED/RELEASE/gpu-mode [runtime flags] -target text take-control
/PINNED/RELEASE/gpu-mode [runtime flags] -target media user-switch
/PINNED/RELEASE/gpu-mode [runtime flags] -target idle return-control
/PINNED/RELEASE/gpu-mode [runtime flags] -target idle recover-user
```

- `take-control` changes supervisor ownership to user ownership. It closes admission, rotates the fence, drains registered work, and verifies the target before committing ownership.
- `user-switch` selects `text`, `media`, or `idle` while user-owned. `return-control` selects the supervisor's workload explicitly. Both authorize terminating all current user work, including queued media work and same-target transfers. They stop both runtime units before starting the selected target.
- `switch` remains a supervisor-only command. It rejects user-owned state.
- User ownership keeps supervisor admission closed. Healthy, stable user execution bypasses lease registration only for the selected workload. The supervisor blocks execution during switching, idle, or an error state.

User execution requests hold a shared cross-process handoff lock. User switches and returns close admission, stop user runtimes and check release, then take the exclusive handoff lock. They wait up to `-drain-timeout` for forwarding handlers to exit before restarting anything. A stalled handler makes the operation fail closed. Cancel the client request or stop the proxy before recovery. All proxies sharing a state database must use this version's handoff locking before enabling ownership commands.

### Recover after a failed transfer or switch

1. Inspect the recorded owner and runtime evidence. Failed or interrupted operations keep the last committed owner. Rollback never restarts stopped user work.
2. Repair the cause. For supervisor ownership, run `recover` with the complete runtime flags.
3. For user ownership, choose the workload to keep and check it. For example:

   ```sh
   /PINNED/RELEASE/gpu-mode [runtime flags] -target media recover-user
   ```

   Use `text` or `idle` instead when appropriate. This verifies without starting or stopping runtimes, preserves user ownership and keeps supervisor admission closed. Idle recovery verifies release under the configured policy. If verification fails, repair or explicitly stop runtimes and retry.
4. Resume only the path appropriate to the verified owner. Do not repeatedly reconcile user-owned state. `reconcile` never takes it over.

Every transfer, user switch and recovery rotates the fence and durably records source and target ownership and the outcome.

## Resolve orphaned work

Use this only after investigating work that cannot report completion, such as a submission with a lost registration token. It stops all running work.

1. Stop every `gpu-workload-proxy` for the state file and wait for shutdown. Do not use older proxies without the lifetime lock.
2. Record an incident reason and resolve the work:

   ```sh
   /PINNED/RELEASE/gpu-mode [runtime flags] -resolve-reason 'operator incident reference' resolve-work
   ```

3. Confirm success. The command requires release of all proxy lifetime locks. It closes admission and rotates the fence. It stops both runtimes. It verifies stopped units and empty workload cgroups. It then atomically marks unfinished work abandoned and records an audit event. Any stop or verification failure leaves work unfinished and admission closed.
4. After successful resolution, run `recover`, restart proxies and select the needed workload.

## Restore state

Restored databases need an explicit `/PINNED/RELEASE/gpu-mode -state /PRIVATE/DURABLE/STATE/state.db restore-state` before any proxy starts. This command needs no runtime flags. It does not copy a backup. Follow the [restore procedure](RESTORING.md) for ordering and validation. Normal restart does not rotate the fence.

<a id="retain-or-archive-audit-history"></a>

## Keep or archive audit history

Audit deletion is explicit. Ordinary proxy work retention does not remove audit
history. Set the retention period, create a consistent SQLite backup using the
[backup procedure](RESTORING.md#back-up-before-an-upgrade), and keep that archive
according to your audit requirements. Then run bounded batches:

```sh
/PINNED/RELEASE/gpu-mode -state /PRIVATE/DURABLE/STATE/state.db \
  -audit-before CUTOFF_RFC3339 -audit-batch 256 prune-audit
```

The cutoff and paths are placeholders. Repeat until `prunedAuditRecords` is zero.
A batch removes at most 256 audit parents (maximum configurable 1024), together
with their events and snapshots. In-progress and current-fence transitions and
transitions referencing unfinished work remain.

The command prunes restoration and work-resolution
history only with no unfinished work or live transition. It always retains
the latest record of each kind. Resolve safety evidence through normal recovery.
Do not delete protected records manually to meet a storage target.

Archive more
often or provision more storage if unresolved evidence keeps growing.
Current-fence transitions stay pinned until a normal switch, ownership change or
recovery rotates the fence. If the workload runs unchanged for a long time, plan
archive and storage checks on your retention schedule. Do not rotate solely to prune.

Pruning holds the controller lock but does not need proxy shutdown: it changes
only eligible audit history, preserving unfinished-work evidence in one SQLite
transaction. Ctrl-C cancels a wait for the controller lock.

Deletion permits SQLite page reuse. It does not shrink the database file. Physical
reclamation needs separate offline maintenance: stop all proxies and controllers,
make a backup, then use SQLite `VACUUM` with adequate temporary disk space. Never replace
or copy a live database file or discard its WAL.
