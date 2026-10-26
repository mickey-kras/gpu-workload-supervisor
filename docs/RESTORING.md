<a id="back-up-restore-and-roll-back-state"></a>

# Make a backup, restore, and roll back state

[Documentation](README.md) | [Repository](../README.md)

<a id="back-up-before-an-upgrade"></a>

## Make a backup before an upgrade

Record the pinned binary version and checksum and complete runtime and proxy configuration
alongside the backup. Stop the controller automation and prevent new execution.
Finish or explicitly stop running jobs, then stop every proxy and workload runtime
using the state file. Keep these components from restarting during maintenance.

Create a consistent SQLite backup using SQLite's backup mechanism or a verified
consistent snapshot. Preserve the complete database, including audit and migration
metadata. Do not copy only the main `.db` file from a live WAL database. Protect
backup contents and the durable state directory with the service identity's private
permissions.

Check the backup integrity and keep the previous binary and configuration
before the new binary or proxy opens state and applies migrations.

Before upgrading, select each media profile's stop policy explicitly through its
catalog `adapter`. The validator no longer accepts the previous implicit choice. The
`systemd` adapter stops the unit and is the only qualified media release path.
The compatibility `media-unload` adapter remains unsupported for live media
because the supervisor cannot currently check release, even with a configured release
endpoint. See the [deployment policy](DEPLOYMENT.md).
`restore-state` and `prune-audit` need no media stop policy.

An ordinary restart against the unchanged database is not a restore. After an
upgrade of supervisor-owned state, reconcile with the new pinned binary and
matching runtime flags before restarting proxies. For user-owned state, inspect
the runtime and explicitly check it with `recover-user`. `reconcile` rejects user
ownership. Use `return-control` only when the operator intends to resume supervisor
ownership. Follow the [ownership and recovery guidance](OPERATIONS.md#ownership-and-unfinished-work)
and validate the deployment before accepting work.

## Restore a backup

The lease incarnation in a SQLite backup can be older than the live database. A restored fence is unsafe until `restore-state` rotates it. This procedure is for an operator restoring a consistent backup, not for an ordinary process restart.

1. Stop every execution proxy, controller automation, and workload runtime using this state database. Wait for admitted jobs to finish or stop them. Prevent another process from starting these components during the restore. The controller lock serializes commands, and `restore-state` also refuses to run while a proxy holds its lifetime lock. Neither lock stops proxies or runtimes or protects the earlier backup replacement.
2. Install a consistent SQLite backup at the configured state path while all processes that use it remain stopped. Use a backup made with SQLite's backup mechanism or another consistent snapshot. Do not copy only the main `.db` file from a live WAL database. Do not leave WAL or SHM files from another database alongside the replacement. Keep the state file in a private directory owned by the service identity.
3. Prepare the restored state, before starting any proxy:

   ```sh
   /PINNED/RELEASE/gpu-mode -state /PRIVATE/DURABLE/STATE/state.db restore-state
   ```

   The database must be an initialized supervisor database. The command rejects an empty placeholder. The command needs no runtime flags and does not copy a backup. On success it prints the closed state with a new incarnation and atomically:

   - Takes supervisor ownership.
   - Sets the active workload to unknown.
   - Closes admission.
   - Marks restored active work abandoned.
   - Invalidates in-progress transitions.
   - Records the restore in audit history.

   Failed work and transition history remain available. If it fails, keep all components stopped and resolve the error before retrying.

   If `restore-state` reports unfinished training, keep all components stopped. If the job ID is unknown, use Python 3's standard-library SQLite reader against the restored state path. Run it as the service identity. This read-only query prints only IDs and recovery categories, never training payloads or tokens:

   ```sh
   python3 - /PRIVATE/DURABLE/STATE/state.db <<'PY'
   import pathlib
   import sqlite3
   import sys

   uri = pathlib.Path(sys.argv[1]).resolve().as_uri() + "?mode=ro"
   with sqlite3.connect(uri, uri=True) as db:
       for job_id, category in db.execute("""
           SELECT id, 'reserved job' FROM training_jobs WHERE finished=0
           UNION ALL
           SELECT id, 'preparing claim' FROM training_claims WHERE status='preparing'
           ORDER BY 1
       """):
           print(job_id, category, sep="\t")
   PY
   ```

   Explicitly recover each listed ID before continuing. As the service identity, use a text editor to create a private regular file with mode `0600` at `/PRIVATE/DURABLE/STATE/training-job-id.txt`. Save exactly one ID (without the tab and category), followed by one newline. Preserve all spaces and do not add quotes or escapes. Paste into the editor, not a terminal input prompt: valid IDs can exceed the terminal's line-length limit.

   Read the file as data. If the file contains exactly one nonempty line, run recovery. Keep the variable expansion quoted:

   ```sh
   if {
       IFS= read -r training_job_id &&
       [ -n "$training_job_id" ] &&
       ! { IFS= read -r extra_line || [ -n "$extra_line" ]; }
   } < /PRIVATE/DURABLE/STATE/training-job-id.txt; then
       /PINNED/RELEASE/gpu-mode [runtime flags] -state /PRIVATE/DURABLE/STATE/state.db -training-job "$training_job_id" training-recover
   else
       printf '%s\n' 'Expected exactly one nonempty ID line; keep components stopped.' >&2
   fi
   ```

   Use the matching runtime configuration and trainer bindings from the restored database's accepted catalog. Runtime commands do not accept `-catalog`. A `reserved job` blocks `restore-state`. Before you retry it, recovery must succeed and you must check recursive cgroup and GPU release. A `preparing claim` does not reserve GPU work and does not block `restore-state`. Recovery records it as `interrupted` without stopping a trainer. The query excludes finished jobs and interrupted claims.

   If the query fails or lists no reserved job despite the unfinished-training error, keep components stopped. Investigate the database path and schema. Do not guess an ID or edit state. After successful recovery, retry `restore-state`, then continue with step 4. Do not bypass the unfinished-training guard. [Training recovery](TRAINING.md#commands) never resumes or relaunches training: it preserves an already persisted terminal outcome, otherwise records `interrupted`. The backup alone does not prove completion. If recovery fails, keep proxies stopped and resolve the runtime or configuration error before retrying.

4. Keep the proxies stopped. Run the same pinned `gpu-mode` with the `reconcile` command and complete runtime flags. Include the same `-state` path. It must finish with a stable, healthy state. If it reports a failure, leave the proxies stopped and use `recover` only after checking the runtime state. If you need an active workload, start it through the controller.
5. Start the proxies only after successful reconciliation. Clients must get the new fence. The proxy rejects requests with a fence from the backup.

`restore-state` acquires the exclusive proxy lifetime lock before opening the database and holds it through completion. Proxies keep their shared locks until in-flight handlers finish. This enforces proxy shutdown only. Operators must still stop all runtimes before replacing the database.

Repeat `restore-state` after every later installation of a backup. Do not run it on a normal restart: reopening an existing database preserves the current lease fence. A restore cannot stop work that already executes outside the proxy, so stopping execution before replacing the database is part of the safety boundary.

## Roll back a release

There is no automatic database downgrade. Never point an older binary at the live
post-upgrade database only because you can replace the executable. You must check the schema and
behavior compatibility for that exact binary and database pair.
The current migration guard rejects a newer schema. Compatible schema
numbers alone do not prove compatible fencing, handoff locking, or completion
semantics. Do not edit migration metadata to force an older binary to open state.

For a supported rollback, stop and block all components as above. Preserve the
failed deployment's database for investigation. Select the already verified
binary and configuration plus its matching pre-upgrade backup. Managed setup backups
also keep accepted owned unit bytes and hashes.

Restore them and refresh the
user manager as described in [Desktop restore and rollback](DESKTOP.md#restore-and-rollback),
protecting modified or foreign unit files. Follow every restore
step above, including `restore-state`, using a binary verified to support that
backup and the restore protocol. Do not mix older proxies without the required
handoff and lifetime locks with a newer controller. Reconcile and check closed-state
recovery before resuming execution.

Refresh client fences after restoration.
Rollback does not recover work and audit updates newer than the backup.
If the previous release cannot safely do this procedure, leave admission
closed and repair forward with a compatible release.
