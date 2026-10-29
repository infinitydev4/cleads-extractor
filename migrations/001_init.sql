-- CLEADS Business Extractor — initial schema.

CREATE TABLE IF NOT EXISTS jobs (
    id           text PRIMARY KEY,
    spec         jsonb       NOT NULL,
    status       text        NOT NULL,
    error        text        NOT NULL DEFAULT '',
    api_key_id   text        NOT NULL DEFAULT '',
    priority     int         NOT NULL DEFAULT 0,
    tasks_total  bigint      NOT NULL DEFAULT 0,
    tasks_done   bigint      NOT NULL DEFAULT 0,
    tasks_failed bigint      NOT NULL DEFAULT 0,
    outstanding  bigint      NOT NULL DEFAULT 0, -- pending + running tasks
    raw_found    bigint      NOT NULL DEFAULT 0,
    uniq         bigint      NOT NULL DEFAULT 0,
    complete     bigint      NOT NULL DEFAULT 0,
    crm_useful   bigint      NOT NULL DEFAULT 0,
    enriched     bigint      NOT NULL DEFAULT 0,
    linked       bigint      NOT NULL DEFAULT 0, -- businesses linked (quota for max_results)
    notified     boolean     NOT NULL DEFAULT false, -- completion event emitted
    created_at   timestamptz NOT NULL DEFAULT now(),
    started_at   timestamptz,
    finished_at  timestamptz
);
CREATE INDEX IF NOT EXISTS jobs_created_idx ON jobs (created_at DESC);
CREATE INDEX IF NOT EXISTS jobs_active_idx ON jobs (status) WHERE status IN ('queued', 'running');
CREATE INDEX IF NOT EXISTS jobs_unnotified_idx ON jobs (id) WHERE NOT notified;

-- The queue. Finished tasks are deleted so the hot table and its partial
-- index stay small; failed tasks are kept for diagnostics.
CREATE TABLE IF NOT EXISTS tasks (
    id          bigserial PRIMARY KEY,
    job_id      text        NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    kind        text        NOT NULL,
    source      text        NOT NULL,
    payload     jsonb       NOT NULL,
    priority    int         NOT NULL DEFAULT 0,
    attempts    int         NOT NULL DEFAULT 0,
    status      text        NOT NULL DEFAULT 'pending',
    run_after   timestamptz NOT NULL DEFAULT now(),
    lease_until timestamptz,
    last_error  text        NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS tasks_ready_idx ON tasks (source, priority DESC, run_after, id) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS tasks_lease_idx ON tasks (lease_until) WHERE status = 'running';
CREATE INDEX IF NOT EXISTS tasks_job_idx ON tasks (job_id);

-- Businesses: indexed columns for filtering, full record in data.
CREATE TABLE IF NOT EXISTS businesses (
    id           bigserial PRIMARY KEY,
    dedup_key    text        NOT NULL,
    name         text        NOT NULL,
    category     text        NOT NULL DEFAULT '',
    city         text        NOT NULL DEFAULT '',
    postal_code  text        NOT NULL DEFAULT '',
    country      text        NOT NULL DEFAULT '',
    phone        text        NOT NULL DEFAULT '',
    website      text        NOT NULL DEFAULT '',
    has_email    boolean     NOT NULL DEFAULT false,
    useful       boolean     NOT NULL DEFAULT false,
    completeness smallint    NOT NULL DEFAULT 0,
    lat          double precision,
    lng          double precision,
    data         jsonb       NOT NULL,
    enriched_at  timestamptz,
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS businesses_postal_idx ON businesses (postal_code);
CREATE INDEX IF NOT EXISTS businesses_city_idx ON businesses (lower(city));
CREATE INDEX IF NOT EXISTS businesses_completeness_idx ON businesses (completeness);

-- Entity-resolution blocking keys (pid:, cid:, siret:, osm:, np:, ng:, ...).
CREATE TABLE IF NOT EXISTS business_keys (
    key         text PRIMARY KEY,
    business_id bigint NOT NULL REFERENCES businesses (id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS business_keys_business_idx ON business_keys (business_id);

-- Which businesses each job found, in discovery order (seq = cursor).
CREATE TABLE IF NOT EXISTS job_businesses (
    job_id      text   NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    business_id bigint NOT NULL REFERENCES businesses (id) ON DELETE CASCADE,
    seq         bigserial,
    PRIMARY KEY (job_id, business_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS job_businesses_seq_idx ON job_businesses (job_id, seq);
