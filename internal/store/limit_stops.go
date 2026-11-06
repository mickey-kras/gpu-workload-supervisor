package store

// schemaV19 adds the limit_stops table. Nothing writes it until the setup
// change for limits. Each time is an INTEGER of Unix nanoseconds in UTC, as
// time.Time.UnixNano gives it, so the CHECKs compare numbers. The store's
// RFC 3339 text has no fixed width, so it cannot keep this order. A stop
// needs a definite time, and the times keep their order: first seen,
// definite, stopping. SQLite cannot add a CHECK to an existing table, so
// these rules are part of v19.
const schemaV19 = `CREATE TABLE limit_stops(
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 workload TEXT NOT NULL CHECK(length(workload) BETWEEN 1 AND 64 AND workload NOT IN ('idle','unknown')),
 first_seen_at INTEGER NOT NULL CHECK(typeof(first_seen_at)='integer' AND first_seen_at > 0),
 definite_at INTEGER CHECK(definite_at IS NULL OR typeof(definite_at)='integer' AND definite_at >= first_seen_at),
 stopping_at INTEGER CHECK(stopping_at IS NULL OR typeof(stopping_at)='integer' AND definite_at IS NOT NULL AND stopping_at >= definite_at));`
