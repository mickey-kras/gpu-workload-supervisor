-- Frozen populated schema v9; do not regenerate from current migrations in tests.
BEGIN TRANSACTION;
CREATE TABLE control_state (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    owner TEXT NOT NULL CHECK (owner IN ('supervisor', 'user')),
    desired_workload TEXT NOT NULL CHECK (desired_workload IN ('text', 'media', 'idle')),
    active_workload TEXT NOT NULL CHECK (active_workload IN ('text', 'media', 'idle', 'unknown')),
    phase TEXT NOT NULL CHECK (phase IN ('stable', 'draining', 'unloading', 'loading', 'verifying', 'reconciling')),
    health TEXT NOT NULL CHECK (health IN ('healthy', 'degraded', 'error')),
    admission TEXT NOT NULL CHECK (admission IN ('open', 'closed')),
    lease_incarnation TEXT NOT NULL,
    lease_epoch INTEGER NOT NULL CHECK (lease_epoch > 0),
    version INTEGER NOT NULL CHECK (version > 0),
    updated_at TEXT NOT NULL
);
INSERT INTO "control_state" VALUES(1,'supervisor','idle','unknown','reconciling','error','closed','fixture',7,11,'2026-09-29T12:00:00Z');
CREATE TABLE registered_work (
    request_id TEXT PRIMARY KEY,
    job_id TEXT,
    lease_incarnation TEXT NOT NULL,
    lease_epoch INTEGER NOT NULL CHECK (lease_epoch > 0),
    registered_at TEXT NOT NULL,
    completed_at TEXT
, workload TEXT CHECK (workload IN ('text', 'media')), completion_outcome TEXT CHECK (completion_outcome IN ('completed', 'abandoned')), registration_token TEXT CHECK (registration_token IS NULL OR length(registration_token) > 0));
INSERT INTO "registered_work" VALUES('unfinished',NULL,'fixture',6,'2026-09-29T12:00:00Z',NULL,'text',NULL,'token-unfinished');
INSERT INTO "registered_work" VALUES('finished',NULL,'fixture',6,'2026-09-29T12:00:00Z','2026-09-29T12:00:00Z','text','completed','token-finished');
CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
INSERT INTO "schema_migrations" VALUES(1,'2026-09-29T12:00:00Z');
INSERT INTO "schema_migrations" VALUES(2,'2026-09-29T12:00:00Z');
INSERT INTO "schema_migrations" VALUES(3,'2026-09-29T12:00:00Z');
INSERT INTO "schema_migrations" VALUES(4,'2026-09-29T12:00:00Z');
INSERT INTO "schema_migrations" VALUES(5,'2026-09-29T12:00:00Z');
INSERT INTO "schema_migrations" VALUES(6,'2026-09-29T12:00:00Z');
INSERT INTO "schema_migrations" VALUES(7,'2026-09-29T12:00:00Z');
INSERT INTO "schema_migrations" VALUES(8,'2026-09-29T12:00:00Z');
INSERT INTO "schema_migrations" VALUES(9,'2026-09-29T12:00:00Z');
CREATE TABLE state_restorations (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    previous_incarnation TEXT NOT NULL,
    previous_epoch INTEGER NOT NULL CHECK (previous_epoch > 0),
    new_incarnation TEXT NOT NULL,
    new_epoch INTEGER NOT NULL CHECK (new_epoch > 0),
    abandoned_work INTEGER NOT NULL CHECK (abandoned_work >= 0),
    invalidated_transitions INTEGER NOT NULL CHECK (invalidated_transitions >= 0),
    created_at TEXT NOT NULL
);
INSERT INTO "state_restorations" VALUES(1,'prior',1,'fixture',7,1,1,'2026-09-29T12:00:00Z');
CREATE TABLE transition_events (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    transition_id TEXT NOT NULL REFERENCES transitions(transition_id),
    phase TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('intent', 'observation')),
    action TEXT NOT NULL,
    outcome TEXT,
    created_at TEXT NOT NULL
);
INSERT INTO "transition_events" VALUES(1,'old','reconciling','observation','fixture','ok','2026-09-29T12:00:00Z');
INSERT INTO "transition_events" VALUES(2,'pending','reconciling','observation','fixture','ok','2026-09-29T12:00:00Z');
INSERT INTO "transition_events" VALUES(3,'live','reconciling','observation','fixture','ok','2026-09-29T12:00:00Z');
CREATE TABLE transition_work (
    transition_id TEXT NOT NULL REFERENCES transitions(transition_id),
    request_id TEXT NOT NULL REFERENCES registered_work(request_id),
    PRIMARY KEY (transition_id, request_id)
);
INSERT INTO "transition_work" VALUES('pending','unfinished');
CREATE TABLE transitions (
    transition_id TEXT PRIMARY KEY,
    lease_incarnation TEXT NOT NULL,
    lease_epoch INTEGER NOT NULL CHECK (lease_epoch > 0),
    source_state BLOB NOT NULL,
    target_state BLOB NOT NULL,
    previous_state BLOB NOT NULL,
    initiator TEXT NOT NULL,
    job_id TEXT,
    phase TEXT NOT NULL,
    deadline TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('in_progress', 'committed', 'failed')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
INSERT INTO "transitions" VALUES('old','fixture',6,'{"owner": "supervisor", "desiredWorkload": "idle", "activeWorkload": "unknown", "phase": "reconciling", "health": "error", "admission": "closed", "leaseFence": {"incarnation": "fixture", "epoch": 7}, "version": 11, "updatedAt": "2026-09-29T12:00:00Z"}','{"owner": "supervisor", "desiredWorkload": "idle", "activeWorkload": "unknown", "phase": "reconciling", "health": "error", "admission": "closed", "leaseFence": {"incarnation": "fixture", "epoch": 7}, "version": 11, "updatedAt": "2026-09-29T12:00:00Z"}','{"owner": "supervisor", "desiredWorkload": "idle", "activeWorkload": "unknown", "phase": "reconciling", "health": "error", "admission": "closed", "leaseFence": {"incarnation": "fixture", "epoch": 7}, "version": 11, "updatedAt": "2026-09-29T12:00:00Z"}','operator',NULL,'reconciling','2026-09-29T12:00:00Z','failed','2026-09-29T12:00:00Z','2026-09-29T12:00:00Z');
INSERT INTO "transitions" VALUES('pending','fixture',6,'{"owner": "supervisor", "desiredWorkload": "idle", "activeWorkload": "unknown", "phase": "reconciling", "health": "error", "admission": "closed", "leaseFence": {"incarnation": "fixture", "epoch": 7}, "version": 11, "updatedAt": "2026-09-29T12:00:00Z"}','{"owner": "supervisor", "desiredWorkload": "idle", "activeWorkload": "unknown", "phase": "reconciling", "health": "error", "admission": "closed", "leaseFence": {"incarnation": "fixture", "epoch": 7}, "version": 11, "updatedAt": "2026-09-29T12:00:00Z"}','{"owner": "supervisor", "desiredWorkload": "idle", "activeWorkload": "unknown", "phase": "reconciling", "health": "error", "admission": "closed", "leaseFence": {"incarnation": "fixture", "epoch": 7}, "version": 11, "updatedAt": "2026-09-29T12:00:00Z"}','operator',NULL,'reconciling','2026-09-29T12:00:00Z','failed','2026-09-29T12:00:00Z','2026-09-29T12:00:00Z');
INSERT INTO "transitions" VALUES('live','fixture',7,'{"owner": "supervisor", "desiredWorkload": "idle", "activeWorkload": "unknown", "phase": "reconciling", "health": "error", "admission": "closed", "leaseFence": {"incarnation": "fixture", "epoch": 7}, "version": 11, "updatedAt": "2026-09-29T12:00:00Z"}','{"owner": "supervisor", "desiredWorkload": "idle", "activeWorkload": "unknown", "phase": "reconciling", "health": "error", "admission": "closed", "leaseFence": {"incarnation": "fixture", "epoch": 7}, "version": 11, "updatedAt": "2026-09-29T12:00:00Z"}','{"owner": "supervisor", "desiredWorkload": "idle", "activeWorkload": "unknown", "phase": "reconciling", "health": "error", "admission": "closed", "leaseFence": {"incarnation": "fixture", "epoch": 7}, "version": 11, "updatedAt": "2026-09-29T12:00:00Z"}','operator',NULL,'reconciling','2026-09-29T12:00:00Z','in_progress','2026-09-29T12:00:00Z','2026-09-29T12:00:00Z');
CREATE TABLE work_resolutions (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    lease_incarnation TEXT NOT NULL,
    lease_epoch INTEGER NOT NULL CHECK (lease_epoch > 0),
    state_version INTEGER NOT NULL CHECK (state_version > 0),
    reason TEXT NOT NULL CHECK (length(reason) BETWEEN 1 AND 512),
    abandoned_work INTEGER NOT NULL CHECK (abandoned_work > 0),
    created_at TEXT NOT NULL
);
INSERT INTO "work_resolutions" VALUES(1,'fixture',6,10,'operator verified release',1,'2026-09-29T12:00:00Z');
CREATE INDEX idx_registered_work_active
ON registered_work(completed_at) WHERE completed_at IS NULL;
CREATE INDEX idx_registered_work_completed_at
ON registered_work(completed_at) WHERE completed_at IS NOT NULL;
CREATE INDEX idx_transition_work_request_id
ON transition_work(request_id);
CREATE INDEX idx_transitions_in_progress_order
ON transitions(created_at, transition_id) WHERE status = 'in_progress';
DELETE FROM "sqlite_sequence";
INSERT INTO "sqlite_sequence" VALUES('transition_events',3);
INSERT INTO "sqlite_sequence" VALUES('state_restorations',1);
INSERT INTO "sqlite_sequence" VALUES('work_resolutions',1);
COMMIT;
