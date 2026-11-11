-- Shared per-registrable-domain politeness gate for distributed workers.
CREATE TABLE IF NOT EXISTS host_limits (
    domain          text PRIMARY KEY,
    next_allowed_at timestamptz NOT NULL DEFAULT now()
);

-- Frontier terminal failure state (no silent delete after max retries).
ALTER TABLE crawler_frontier
    ADD COLUMN IF NOT EXISTS state text NOT NULL DEFAULT 'pending',
    ADD COLUMN IF NOT EXISTS last_error text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS politeness_domain text NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS crawler_frontier_state_idx
    ON crawler_frontier (state) WHERE state <> 'pending';
CREATE INDEX IF NOT EXISTS crawler_frontier_politeness_domain_idx
    ON crawler_frontier (politeness_domain);
