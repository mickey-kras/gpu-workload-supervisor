# Back up, restore, and roll back state

## Back up before an upgrade

Record the pinned binary version/checksum and complete runtime/proxy configuration
alongside the backup. Stop controller automation and prevent new execution;
finish or explicitly stop running jobs, then stop every proxy and workload runtime
using the state file. Keep these components from restarting during maintenance.
Create a consistent SQLite backup using SQLite's backup mechanism or a verified
consistent snapshot; preserve the complete database, including audit and migration
metadata. Do not copy only the main `.db` file from a live WAL database. Protect
backup contents and the durable state directory with the service identity's private
permissions. Verify backup integrity and retain the previous binary/configuration
before the new binary or proxy opens state and applies migrations.

An ordinary restart against the unchanged database is not a restore. After an
upgrade of supervisor-owned state, reconcile with the new pinned binary and
matching runtime flags before restarting proxies. For user-owned state, inspect
the runtime and explicitly verify it with `recover-user`; `reconcile` rejects user
ownership. Use `return-control` only when the operator intends to resume supervisor
ownership. Follow the [ownership and recovery guidance](../README.md#ownership-and-unfinished-work)
and validate the deployment before accepting work.

## Restore a backup

The lease incarnation in a SQLite backup may be older than the live database. A restored fence is unsafe until `restore-state` rotates it. This procedure is for an operator restoring a consistent backup, not for an ordinary process restart.

1. Stop every execution proxy, controller automation, and workload runtime using this state database. Wait for admitted jobs to finish or stop them. Prevent another process from starting these components during the restore. The file lock coordinates controller commands, but it does not stop a running proxy or runtime.
2. Install a consistent SQLite backup at the configured state path while all processes using it are stopped. Use a backup made with SQLite's backup mechanism or another consistent snapshot. Do not copy only the main `.db` file from a live WAL database or leave WAL/SHM files from another database alongside the replacement. Keep the state file in a private directory owned by the service identity.
3. Prepare the restored state, before starting any proxy:

   ```sh
   /PINNED/RELEASE/gpu-mode -state /PRIVATE/DURABLE/STATE/state.db restore-state
   ```

   The database must be an initialized supervisor database; an empty placeholder is rejected. The command needs no runtime flags and does not copy a backup. On success it prints the closed state with a new incarnation. It atomically takes supervisor ownership, sets the active workload to unknown, closes admission, marks restored active work abandoned, invalidates in-progress transitions, and records counts and old/new fences in `state_restorations`. Failed work and transitions remain in the audit tables. If it fails, keep all components stopped and resolve the error before retrying.

4. With the proxies still stopped, run the same pinned `gpu-mode` with the complete runtime flags, including the same `-state` path, and the `reconcile` command. It must finish with a stable, healthy state. If it reports a failure, leave the proxies stopped and use `recover` only after checking the runtime state. Start a workload through the controller if needed.
5. Start the proxies only after successful reconciliation. Clients must obtain the new fence; requests carrying a fence from the backup are rejected.

Repeat `restore-state` after every subsequent installation of a backup. Do not run it on a normal restart: reopening an existing database preserves the current lease fence. A restore cannot stop work that is already executing outside the proxy, so stopping execution before replacing the database is part of the safety boundary.

## Roll back a release

There is no automatic database downgrade. Never point an older binary at the live
post-upgrade database merely because the executable can be replaced. Schema and
behavior compatibility must be verified for that exact binary/database pair;
a newer schema is rejected by the current migration guard. Compatible schema
numbers alone do not establish compatible fencing, handoff locking, or completion
semantics. Do not edit migration metadata to force an older binary to open state.

For a supported rollback, stop and block all components as above, preserve the
failed deployment's database for investigation, and select the previously verified
binary/configuration plus its matching pre-upgrade backup. Follow every restore
step above, including `restore-state`, using a binary verified to support that
backup and the restore protocol. Do not mix older proxies without the required
handoff/lifetime locks with a newer controller. Reconcile and verify closed-state
recovery before resuming execution; refresh client fences after restoration.
Work and audit updates newer than the backup are not recovered by rollback.
If the previous release cannot safely perform this procedure, leave admission
closed and repair forward with a compatible release.
