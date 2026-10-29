-- CLEADS Business Extractor — diagnostics readable from any API replica.

-- Warnings and errors of every process (API and workers). No foreign key to
-- jobs: an event must never fail to insert, whatever happened to its job.
-- Rows older than EVENTS_RETENTION are purged periodically.
CREATE TABLE IF NOT EXISTS events (
    id      bigserial   PRIMARY KEY,
    ts      timestamptz NOT NULL,
    level   text        NOT NULL,
    msg     text        NOT NULL,
    job_id  text        NOT NULL DEFAULT '',
    task_id bigint,
    source  text        NOT NULL DEFAULT '',
    worker  text        NOT NULL DEFAULT '',
    attrs   jsonb       NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS events_job_ts_idx ON events (job_id, ts);
CREATE INDEX IF NOT EXISTS events_ts_idx ON events (ts);

-- Worker heartbeats: rate governors and circuit breakers live in each worker
-- process, so workers publish their state here for the health summary.
CREATE TABLE IF NOT EXISTS workers (
    id         text        PRIMARY KEY,
    role       text        NOT NULL,
    version    text        NOT NULL DEFAULT '',
    started_at timestamptz NOT NULL,
    seen_at    timestamptz NOT NULL,
    sources    jsonb       NOT NULL DEFAULT '[]'
);
