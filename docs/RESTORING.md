# Restore a state database

The lease incarnation in a SQLite backup may be older than the live database. A restored fence is unsafe until `restore-state` rotates it. This procedure is for an operator restoring a consistent backup, not for an ordinary process restart.

1. Stop every execution proxy, controller automation, and workload runtime using this state database. Wait for admitted jobs to finish or stop them. Prevent another process from starting these components during the restore. The file lock coordinates controller commands, but it does not stop a running proxy or runtime.
2. Install a consistent SQLite backup at the configured state path while all processes using it are stopped. Use a backup made with SQLite's backup mechanism or another consistent snapshot. Do not copy only the main `.db` file from a live WAL database or leave WAL/SHM files from another database alongside the replacement. Keep the state file in a private directory owned by the service identity.
3. Prepare the restored state, before starting any proxy:

   ```sh
   gpu-mode -state /PRIVATE/PATH/state.db restore-state
   ```

   The database must already exist. The command needs no runtime flags and does not copy a backup. On success it prints the closed state with a new incarnation. It atomically takes supervisor ownership, sets the active workload to unknown, closes admission, marks restored active work abandoned, invalidates in-progress transitions, and records counts and old/new fences in `state_restorations`. Failed work and transitions remain in the audit tables. If it fails, keep all components stopped and resolve the error before retrying.

4. With the proxies still stopped, run `gpu-mode` with the normal runtime flags and the `reconcile` command. It must finish with a stable, healthy state. If it reports a failure, leave the proxies stopped and use `recover` only after checking the runtime state. Start a workload through the controller if needed.
5. Start the proxies only after successful reconciliation. Clients must obtain the new fence; requests carrying a fence from the backup are rejected.

Repeat `restore-state` after every subsequent installation of a backup. Do not run it on a normal restart: reopening an existing database preserves the current lease fence. A restore cannot stop work that is already executing outside the proxy, so stopping execution before replacing the database is part of the safety boundary.
