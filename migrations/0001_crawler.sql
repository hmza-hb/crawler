-- Documents: one row per distinct page we have fetched, keyed by URL, with the
-- metadata needed to make the next fetch conditional and cheap.
CREATE TABLE IF NOT EXISTS crawler_documents (
    id               text        PRIMARY KEY DEFAULT gen_random_uuid()::text,
    url              text        NOT NULL,
    requested_url     text        NOT NULL,
    redirect_chain    text[]      NOT NULL DEFAULT '{}',
    status           integer     NOT NULL DEFAULT 0,
    status_text      text        NOT NULL DEFAULT '',
    content_type     text        NOT NULL DEFAULT '',
    charset          text        NOT NULL DEFAULT '',
    content_length   bigint      NOT NULL DEFAULT -1,
    etag             text        NOT NULL DEFAULT '',
    last_modified    timestamptz,
    content_hash     text        NOT NULL DEFAULT '',
    raw_hash         text        NOT NULL DEFAULT '',
    body             bytea,
    body_truncated   boolean     NOT NULL DEFAULT false,
    not_modified     boolean     NOT NULL DEFAULT false,
    filtered         boolean     NOT NULL DEFAULT false,
    depth            integer     NOT NULL DEFAULT 0,
    fetched_at       timestamptz NOT NULL,
    elapsed_ms       bigint      NOT NULL DEFAULT 0,
    attempt          integer     NOT NULL DEFAULT 1,
    user_agent       text        NOT NULL DEFAULT '',
    source_hint      text        NOT NULL DEFAULT '',
    run_id           text        NOT NULL DEFAULT '',
    links            jsonb       NOT NULL DEFAULT '[]'::jsonb,

    CONSTRAINT crawler_documents_content_length_valid CHECK (content_length >= -1),
    CONSTRAINT crawler_documents_depth_valid CHECK (depth >= 0)
);

-- One row per URL. Saving a repeat fetch of the same bytes updates this row
-- instead of inserting, so a daily crawl of a static site does not grow the
-- table by a row per URL per day.
CREATE UNIQUE INDEX IF NOT EXISTS crawler_documents_url_key ON crawler_documents (url);
CREATE INDEX IF NOT EXISTS crawler_documents_run_id_idx ON crawler_documents (run_id);
CREATE INDEX IF NOT EXISTS crawler_documents_fetched_at_idx ON crawler_documents (fetched_at DESC);
CREATE INDEX IF NOT EXISTS crawler_documents_content_hash_idx ON crawler_documents (content_hash);
CREATE INDEX IF NOT EXISTS crawler_documents_status_idx ON crawler_documents (status);

-- Crawl runs, so a caller can see what a crawl cost and why it stopped.
CREATE TABLE IF NOT EXISTS crawler_runs (
    run_id         text        PRIMARY KEY,
    started_at     timestamptz NOT NULL DEFAULT now(),
    finished_at    timestamptz,
    depth          text        NOT NULL DEFAULT '',
    stop_reason    text        NOT NULL DEFAULT '',
    pages_fetched  integer     NOT NULL DEFAULT 0,
    bytes_retained bigint      NOT NULL DEFAULT 0,
    skipped        integer     NOT NULL DEFAULT 0,
    failed         integer     NOT NULL DEFAULT 0,
    not_modified   integer     NOT NULL DEFAULT 0,
    filtered       integer     NOT NULL DEFAULT 0,
    duration_ms    bigint      NOT NULL DEFAULT 0
);

-- The frontier, durable so a crash mid-crawl does not lose queued work and so
-- several workers can share one queue.
CREATE TABLE IF NOT EXISTS crawler_frontier (
    id           bigserial   PRIMARY KEY,
    dedupe_key   text        NOT NULL,
    url          text        NOT NULL,
    depth        integer     NOT NULL DEFAULT 0,
    priority     integer     NOT NULL DEFAULT 0,
    source_hint  text        NOT NULL DEFAULT '',
    run_id       text        NOT NULL DEFAULT '',
    attempts     integer     NOT NULL DEFAULT 0,
    failures     integer     NOT NULL DEFAULT 0,
    enqueued_at  timestamptz NOT NULL DEFAULT now(),
    ready_at     timestamptz NOT NULL DEFAULT now(),
    leased_until timestamptz,

    CONSTRAINT crawler_frontier_depth_valid CHECK (depth >= 0)
);

-- Keys this run has already accepted, kept after the queue row is deleted.
-- Without it a completed item could be re-enqueued by a link from a page
-- fetched later, and A -> B -> A would crawl in circles forever. This table is
-- small and is cleared per run.
CREATE TABLE IF NOT EXISTS crawler_frontier_seen (
    run_id     text        NOT NULL,
    dedupe_key text        NOT NULL,
    seen_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, dedupe_key)
);

-- Claim scans this index to find the highest-priority ready item. The partial
-- predicate keeps completed work out of the queue entirely.
CREATE INDEX IF NOT EXISTS crawler_frontier_claim_idx
    ON crawler_frontier (priority DESC, enqueued_at);
CREATE INDEX IF NOT EXISTS crawler_frontier_lease_idx
    ON crawler_frontier (leased_until) WHERE leased_until IS NOT NULL;
CREATE INDEX IF NOT EXISTS crawler_frontier_run_idx ON crawler_frontier (run_id);
