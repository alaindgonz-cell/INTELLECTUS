//! SQLite persistence: append-only events, immutable snapshots, head pointer,
//! content-addressed blobs and action/outbox projections.
//!
//! One `commit` is one SQLite transaction: check head, append events, write
//! snapshots and projections, advance the head. The append-only property is
//! enforced inside the application's trust model (triggers), not against an
//! administrator who controls the host.

use std::path::Path;

use rusqlite::{params, Connection, OptionalExtension, TransactionBehavior};

use crate::canonical::to_canonical;
use crate::error::{CoreError, Result};
use crate::state::{Action, Event, State};

const SCHEMA: &str = r#"
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
    project_id      TEXT    NOT NULL,
    sequence        INTEGER NOT NULL CHECK (sequence > 0),
    event_id        TEXT    NOT NULL UNIQUE,
    event_type      TEXT    NOT NULL,
    reducer_version INTEGER NOT NULL,
    body            TEXT    NOT NULL,
    PRIMARY KEY (project_id, sequence)
);
CREATE TABLE IF NOT EXISTS snapshots (
    project_id   TEXT    NOT NULL,
    sequence     INTEGER NOT NULL,
    state_digest TEXT    NOT NULL,
    state        TEXT    NOT NULL,
    PRIMARY KEY (project_id, sequence),
    FOREIGN KEY (project_id, sequence) REFERENCES events (project_id, sequence)
);
CREATE TABLE IF NOT EXISTS head (
    project_id   TEXT    PRIMARY KEY,
    sequence     INTEGER NOT NULL,
    state_digest TEXT    NOT NULL,
    FOREIGN KEY (project_id, sequence) REFERENCES snapshots (project_id, sequence)
);
CREATE TABLE IF NOT EXISTS blobs (
    digest TEXT PRIMARY KEY,
    bytes  BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS actions (
    project_id       TEXT    NOT NULL,
    action_id        TEXT    NOT NULL,
    idempotency_key  TEXT    NOT NULL,
    action_digest    TEXT    NOT NULL,
    created_sequence INTEGER NOT NULL,
    PRIMARY KEY (project_id, action_id),
    UNIQUE (project_id, idempotency_key),
    FOREIGN KEY (project_id, created_sequence) REFERENCES events (project_id, sequence)
);
CREATE TABLE IF NOT EXISTS outbox (
    project_id       TEXT    NOT NULL,
    action_id        TEXT    NOT NULL,
    dispatch_state   TEXT    NOT NULL,
    attempt_id       TEXT,
    updated_sequence INTEGER NOT NULL,
    PRIMARY KEY (project_id, action_id),
    FOREIGN KEY (project_id, action_id) REFERENCES actions (project_id, action_id),
    FOREIGN KEY (project_id, updated_sequence) REFERENCES events (project_id, sequence)
);
CREATE TRIGGER IF NOT EXISTS events_no_update BEFORE UPDATE ON events
BEGIN SELECT RAISE(ABORT, 'events are append-only'); END;
CREATE TRIGGER IF NOT EXISTS events_no_delete BEFORE DELETE ON events
BEGIN SELECT RAISE(ABORT, 'events are append-only'); END;
CREATE TRIGGER IF NOT EXISTS snapshots_no_update BEFORE UPDATE ON snapshots
BEGIN SELECT RAISE(ABORT, 'snapshots are immutable'); END;
CREATE TRIGGER IF NOT EXISTS snapshots_no_delete BEFORE DELETE ON snapshots
BEGIN SELECT RAISE(ABORT, 'snapshots are immutable'); END;
CREATE TRIGGER IF NOT EXISTS blobs_no_update BEFORE UPDATE ON blobs
BEGIN SELECT RAISE(ABORT, 'blobs are immutable'); END;
"#;

pub struct Store {
    conn: Connection,
    pub project_id: String,
}

/// Everything a single coordinator transaction writes.
pub struct Batch {
    pub base_sequence: u64,
    pub events: Vec<(Event, String, String)>, // (event, resulting digest, resulting state json)
    pub blobs: Vec<(String, Vec<u8>)>,
    pub actions: Vec<(Action, u64)>, // changed actions with their updating sequence
}

impl Store {
    fn configure(conn: &Connection) -> Result<()> {
        conn.execute_batch(
            "PRAGMA foreign_keys = ON; PRAGMA journal_mode = WAL; PRAGMA synchronous = FULL;",
        )?;
        let fk: i64 = conn.query_row("PRAGMA foreign_keys", [], |r| r.get(0))?;
        if fk != 1 {
            return Err(CoreError::new(
                "STORAGE",
                "foreign keys could not be enabled",
            ));
        }
        conn.execute_batch(SCHEMA)?;
        Ok(())
    }

    /// Create a new database for `project_id`. Fails if one already exists.
    pub fn create(path: &Path, project_id: &str) -> Result<Store> {
        let conn = Connection::open(path)?;
        Self::configure(&conn)?;
        let existing: Option<String> = conn
            .query_row("SELECT value FROM meta WHERE key = 'project_id'", [], |r| {
                r.get(0)
            })
            .optional()?;
        if existing.is_some() {
            return Err(CoreError::new(
                "ALREADY_INITIALIZED",
                "database already holds a project",
            ));
        }
        conn.execute(
            "INSERT INTO meta (key, value) VALUES ('project_id', ?1)",
            params![project_id],
        )?;
        Ok(Store {
            conn,
            project_id: project_id.to_string(),
        })
    }

    pub fn open(path: &Path) -> Result<Store> {
        if !path.exists() {
            return Err(CoreError::new(
                "NOT_INITIALIZED",
                format!("{} does not exist", path.display()),
            ));
        }
        let conn = Connection::open(path)?;
        Self::configure(&conn)?;
        let project_id: String = conn
            .query_row("SELECT value FROM meta WHERE key = 'project_id'", [], |r| {
                r.get(0)
            })
            .optional()?
            .ok_or_else(|| CoreError::new("NOT_INITIALIZED", "no project in database"))?;
        Ok(Store { conn, project_id })
    }

    pub fn head(&self) -> Result<Option<(u64, String)>> {
        Ok(self
            .conn
            .query_row(
                "SELECT sequence, state_digest FROM head WHERE project_id = ?1",
                params![self.project_id],
                |r| Ok((r.get::<_, i64>(0)? as u64, r.get(1)?)),
            )
            .optional()?)
    }

    pub fn load_snapshot(&self, sequence: u64) -> Result<(State, String)> {
        let (json, digest): (String, String) = self.conn.query_row(
            "SELECT state, state_digest FROM snapshots WHERE project_id = ?1 AND sequence = ?2",
            params![self.project_id, sequence as i64],
            |r| Ok((r.get(0)?, r.get(1)?)),
        )?;
        let state: State = serde_json::from_str(&json)
            .map_err(|e| CoreError::new("INTEGRITY", format!("snapshot decode: {e}")))?;
        if state.digest()? != digest {
            return Err(CoreError::new(
                "INTEGRITY",
                format!("snapshot {sequence} digest mismatch"),
            ));
        }
        Ok((state, digest))
    }

    pub fn snapshot_digest(&self, sequence: u64) -> Result<String> {
        Ok(self.conn.query_row(
            "SELECT state_digest FROM snapshots WHERE project_id = ?1 AND sequence = ?2",
            params![self.project_id, sequence as i64],
            |r| r.get(0),
        )?)
    }

    pub fn events(&self) -> Result<Vec<Event>> {
        let mut stmt = self
            .conn
            .prepare("SELECT body FROM events WHERE project_id = ?1 ORDER BY sequence ASC")?;
        let rows = stmt.query_map(params![self.project_id], |r| r.get::<_, String>(0))?;
        let mut out = Vec::new();
        for row in rows {
            let body = row?;
            out.push(
                serde_json::from_str::<Event>(&body).map_err(|e| {
                    CoreError::new("UNSUPPORTED_SCHEMA", format!("event decode: {e}"))
                })?,
            );
        }
        Ok(out)
    }

    pub fn blob(&self, digest: &str) -> Result<Option<Vec<u8>>> {
        Ok(self
            .conn
            .query_row(
                "SELECT bytes FROM blobs WHERE digest = ?1",
                params![digest],
                |r| r.get(0),
            )
            .optional()?)
    }

    pub fn commit(&mut self, batch: &Batch) -> Result<()> {
        if batch.events.is_empty() {
            return Ok(());
        }
        let tx = self
            .conn
            .transaction_with_behavior(TransactionBehavior::Immediate)?;
        let head: Option<i64> = tx
            .query_row(
                "SELECT sequence FROM head WHERE project_id = ?1",
                params![self.project_id],
                |r| r.get(0),
            )
            .optional()?;
        if head.unwrap_or(0) as u64 != batch.base_sequence {
            return Err(CoreError::new(
                "HEAD_MOVED",
                "head changed since the transaction began",
            ));
        }
        for (digest, bytes) in &batch.blobs {
            tx.execute(
                "INSERT OR IGNORE INTO blobs (digest, bytes) VALUES (?1, ?2)",
                params![digest, bytes],
            )?;
        }
        let mut last = None;
        for (event, digest, state_json) in &batch.events {
            tx.execute(
                "INSERT INTO events (project_id, sequence, event_id, event_type, reducer_version, body)
                 VALUES (?1, ?2, ?3, ?4, ?5, ?6)",
                params![
                    event.project_id,
                    event.sequence as i64,
                    event.event_id,
                    event.event_type,
                    event.reducer_version,
                    to_canonical(event)?
                ],
            )?;
            tx.execute(
                "INSERT INTO snapshots (project_id, sequence, state_digest, state) VALUES (?1, ?2, ?3, ?4)",
                params![event.project_id, event.sequence as i64, digest, state_json],
            )?;
            last = Some((event.sequence, digest.clone()));
        }
        for (action, seq) in &batch.actions {
            let state = serde_json::to_value(action.state).unwrap_or_default();
            tx.execute(
                "INSERT OR IGNORE INTO actions (project_id, action_id, idempotency_key, action_digest, created_sequence)
                 VALUES (?1, ?2, ?3, ?4, ?5)",
                params![self.project_id, action.action_id, action.idempotency_key, action.action_digest, *seq as i64],
            )?;
            // Enforce the scoped idempotency binding at the storage layer too.
            let bound: String = tx.query_row(
                "SELECT action_id FROM actions WHERE project_id = ?1 AND idempotency_key = ?2",
                params![self.project_id, action.idempotency_key],
                |r| r.get(0),
            )?;
            if bound != action.action_id {
                return Err(CoreError::new(
                    "IDEMPOTENCY_CONFLICT",
                    "idempotency key bound to another action",
                ));
            }
            tx.execute(
                "INSERT INTO outbox (project_id, action_id, dispatch_state, attempt_id, updated_sequence)
                 VALUES (?1, ?2, ?3, ?4, ?5)
                 ON CONFLICT (project_id, action_id) DO UPDATE SET
                   dispatch_state = excluded.dispatch_state,
                   attempt_id = excluded.attempt_id,
                   updated_sequence = excluded.updated_sequence",
                params![
                    self.project_id,
                    action.action_id,
                    state.as_str().unwrap_or("UNKNOWN"),
                    action.attempts.last().map(|a| a.attempt_id.clone()),
                    *seq as i64
                ],
            )?;
        }
        let (seq, digest) = last.expect("non-empty batch");
        tx.execute(
            "INSERT INTO head (project_id, sequence, state_digest) VALUES (?1, ?2, ?3)
             ON CONFLICT (project_id) DO UPDATE SET sequence = excluded.sequence, state_digest = excluded.state_digest",
            params![self.project_id, seq as i64, digest],
        )?;
        tx.commit()?;
        Ok(())
    }

    /// Raw connection access for adversarial tests only.
    #[doc(hidden)]
    pub fn raw(&self) -> &Connection {
        &self.conn
    }
}
