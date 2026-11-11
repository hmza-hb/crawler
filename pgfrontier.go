package crawler

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresFrontier is a durable, shareable Frontier. Several crawler processes
// can claim from the same queue, and a process that dies mid-fetch leaves a
// lease that expires rather than a permanently stuck item.
type PostgresFrontier struct {
	pool         *pgxpool.Pool
	now          func() time.Time
	perHostDelay time.Duration
}

// NewPostgresFrontier wraps a pool.
func NewPostgresFrontier(pool *pgxpool.Pool) *PostgresFrontier {
	return &PostgresFrontier{pool: pool, now: time.Now}
}

// SetPerHostDelay configures the shared Postgres politeness gate (distributed
// instances). Zero disables the host_limits check.
func (f *PostgresFrontier) SetPerHostDelay(d time.Duration) {
	f.perHostDelay = d
}

// SetClock replaces the time source, for deterministic tests.
func (f *PostgresFrontier) SetClock(now func() time.Time) { f.now = now }

func politenessDomain(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return registrableDomain(u.Hostname())
}

// Enqueue implements Frontier.
func (f *PostgresFrontier) Enqueue(ctx context.Context, items []Item) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}
	batch := &pgx.Batch{}
	now := f.now().UTC()
	for _, it := range items {
		domain := politenessDomain(it.URL)
		batch.Queue(`
			WITH seen AS (
				INSERT INTO crawler_frontier_seen (run_id, dedupe_key, seen_at)
				VALUES ($1, $2, $3)
				ON CONFLICT (run_id, dedupe_key) DO NOTHING
				RETURNING 1
			)
			INSERT INTO crawler_frontier (
				dedupe_key, url, depth, priority, source_hint, run_id, enqueued_at, ready_at, politeness_domain
			)
			SELECT $2, $4, $5, $6, $7, $1, $3, $3, $8
			FROM seen`,
			it.RunID, itemKey(it), now, it.URL, it.Depth, it.Priority, it.SourceHint, domain)
	}
	br := f.pool.SendBatch(ctx, batch)
	added := 0
	for _, it := range items {
		tags, err := br.Exec()
		if err != nil {
			_ = br.Close()
			return added, fmt.Errorf("crawler: enqueue %s: %w", it.URL, err)
		}
		if tags.RowsAffected() == 1 {
			added++
		}
	}
	if err := br.Close(); err != nil {
		return added, fmt.Errorf("crawler: close enqueue batch: %w", err)
	}
	return added, nil
}

func itemKey(it Item) string {
	if it.Key != "" {
		return it.Key
	}
	return it.URL
}

// Claim implements Frontier with FOR UPDATE SKIP LOCKED, so two workers never
// lease the same item. An item whose lease has expired is claimable again.
func (f *PostgresFrontier) Claim(ctx context.Context, n int, lease time.Duration) ([]Item, error) {
	if n <= 0 {
		return nil, nil
	}
	now := f.now().UTC()
	if f.perHostDelay <= 0 {
		return f.claimSimple(ctx, n, lease, now)
	}
	return f.claimWithHostLimits(ctx, n, lease, now)
}

func (f *PostgresFrontier) claimSimple(ctx context.Context, n int, lease time.Duration, now time.Time) ([]Item, error) {
	rows, err := f.pool.Query(ctx, `
		WITH claimable AS (
			SELECT id FROM crawler_frontier
			WHERE state = 'pending'
			  AND ready_at <= $1
			  AND (leased_until IS NULL OR leased_until <= $1)
			ORDER BY priority DESC, enqueued_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		UPDATE crawler_frontier f
		SET leased_until = $3, attempts = f.attempts + 1
		FROM claimable c
		WHERE f.id = c.id
		RETURNING f.dedupe_key, f.url, f.depth, f.priority, f.source_hint, f.run_id,
		          f.attempts, f.enqueued_at`, now, n, now.Add(lease))
	if err != nil {
		return nil, fmt.Errorf("crawler: claim: %w", err)
	}
	return scanClaimedSimple(rows, n)
}

func (f *PostgresFrontier) claimWithHostLimits(ctx context.Context, n int, lease time.Duration, now time.Time) ([]Item, error) {
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("crawler: claim tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT f.id, f.politeness_domain
		FROM crawler_frontier f
		LEFT JOIN host_limits hl ON hl.domain = f.politeness_domain AND f.politeness_domain <> ''
		WHERE f.state = 'pending'
		  AND f.ready_at <= $1
		  AND (f.leased_until IS NULL OR f.leased_until <= $1)
		  AND (
		    f.politeness_domain = ''
		    OR hl.next_allowed_at IS NULL
		    OR hl.next_allowed_at <= $1
		  )
		ORDER BY f.politeness_domain, f.priority DESC, f.enqueued_at
		FOR UPDATE OF f SKIP LOCKED`, now)
	if err != nil {
		return nil, fmt.Errorf("crawler: claim scan: %w", err)
	}
	defer rows.Close()

	type pick struct {
		id     int64
		domain string
	}
	seenDomain := make(map[string]struct{})
	ids := make([]int64, 0, n)
	for rows.Next() {
		var id int64
		var domain string
		if err := rows.Scan(&id, &domain); err != nil {
			return nil, fmt.Errorf("crawler: claim row: %w", err)
		}
		if domain != "" {
			if _, ok := seenDomain[domain]; ok {
				continue
			}
			seenDomain[domain] = struct{}{}
		}
		ids = append(ids, id)
		if len(ids) >= n {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return nil, nil
	}

	claimRows, err := tx.Query(ctx, `
		UPDATE crawler_frontier f
		SET leased_until = $2, attempts = f.attempts + 1
		WHERE f.id = ANY($1)
		RETURNING f.dedupe_key, f.url, f.depth, f.priority, f.source_hint, f.run_id,
		          f.attempts, f.enqueued_at, f.politeness_domain`,
		ids, now.Add(lease))
	if err != nil {
		return nil, fmt.Errorf("crawler: claim update: %w", err)
	}
	out, domains, err := scanClaimedWithDomains(claimRows, n)
	if err != nil {
		return nil, err
	}

	nextAllowed := now.Add(f.perHostDelay)
	for _, domain := range domains {
		if domain == "" {
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO host_limits (domain, next_allowed_at)
			VALUES ($1, $2)
			ON CONFLICT (domain) DO UPDATE
			SET next_allowed_at = GREATEST(host_limits.next_allowed_at, EXCLUDED.next_allowed_at)`,
			domain, nextAllowed); err != nil {
			return nil, fmt.Errorf("crawler: host limit %q: %w", domain, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("crawler: claim commit: %w", err)
	}
	return out, nil
}

func scanClaimedSimple(rows pgx.Rows, n int) ([]Item, error) {
	defer rows.Close()
	out := make([]Item, 0, n)
	for rows.Next() {
		var (
			it       Item
			attempts int
		)
		if err := rows.Scan(&it.Key, &it.URL, &it.Depth, &it.Priority, &it.SourceHint,
			&it.RunID, &attempts, &it.EnqueuedAt); err != nil {
			return nil, fmt.Errorf("crawler: scan claim: %w", err)
		}
		it.Attempts = attempts
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("crawler: read claim rows: %w", err)
	}
	sortItems(out)
	return out, nil
}

func scanClaimedWithDomains(rows pgx.Rows, n int) ([]Item, []string, error) {
	defer rows.Close()
	out := make([]Item, 0, n)
	domains := make([]string, 0, n)
	for rows.Next() {
		var (
			it       Item
			attempts int
			domain   string
		)
		if err := rows.Scan(&it.Key, &it.URL, &it.Depth, &it.Priority, &it.SourceHint,
			&it.RunID, &attempts, &it.EnqueuedAt, &domain); err != nil {
			return nil, nil, fmt.Errorf("crawler: scan claim: %w", err)
		}
		it.Attempts = attempts
		out = append(out, it)
		if domain != "" {
			domains = append(domains, domain)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("crawler: read claim rows: %w", err)
	}
	sortItems(out)
	return out, domains, nil
}

// Complete implements Frontier. A nil error deletes the item; an error backs it
// off exponentially, and an item that keeps failing moves to state failed.
func (f *PostgresFrontier) Complete(ctx context.Context, items []Item, err error) error {
	if len(items) == 0 {
		return nil
	}
	now := f.now().UTC()
	for _, it := range items {
		if err == nil {
			if _, delErr := f.pool.Exec(ctx,
				`DELETE FROM crawler_frontier WHERE run_id = $1 AND dedupe_key = $2`,
				it.RunID, itemKey(it)); delErr != nil {
				return fmt.Errorf("crawler: complete %s: %w", it.URL, delErr)
			}
			continue
		}
		errText := err.Error()
		if strings.Contains(errText, errRelease.Error()) {
			errText = "released for retry"
		}
		if _, updErr := f.pool.Exec(ctx, `
			UPDATE crawler_frontier
			SET failures     = failures + 1,
			    leased_until = NULL,
			    ready_at     = $3 + make_interval(secs => LEAST(300, (failures + 1) * 2)),
			    last_error   = $4,
			    state        = CASE WHEN failures + 1 >= 3 THEN 'failed' ELSE state END
			WHERE run_id = $1 AND dedupe_key = $2`,
			it.RunID, itemKey(it), now, errText); updErr != nil {
			return fmt.Errorf("crawler: release %s: %w", it.URL, updErr)
		}
	}
	return nil
}

// FailedCount returns frontier rows in terminal failed state.
func (f *PostgresFrontier) FailedCount(ctx context.Context, runID string) (int, error) {
	var n int
	q := `SELECT count(*) FROM crawler_frontier WHERE state = 'failed'`
	args := []any{}
	if runID != "" {
		q += ` AND run_id = $1`
		args = append(args, runID)
	}
	if err := f.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("crawler: failed count: %w", err)
	}
	return n, nil
}

// Depth implements Frontier.
func (f *PostgresFrontier) Depth(ctx context.Context) (int, error) {
	var n int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM crawler_frontier WHERE state <> 'failed'`).Scan(&n); err != nil {
		return 0, fmt.Errorf("crawler: frontier depth: %w", err)
	}
	return n, nil
}

// Pending implements Frontier. It is one query rather than a count plus a scan,
// so a supervisor does not have to guess how long to sleep.
func (f *PostgresFrontier) Pending(ctx context.Context) (time.Duration, bool, error) {
	var (
		total       int
		ready       int
		leasedCount int
		nextReady   *time.Time
	)
	err := f.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE state <> 'failed'),
		       count(*) FILTER (WHERE state = 'pending' AND ready_at <= $1 AND (leased_until IS NULL OR leased_until <= $1)),
		       count(*) FILTER (WHERE state = 'pending' AND leased_until > $1),
		       min(ready_at) FILTER (WHERE state = 'pending' AND ready_at > $1 AND (leased_until IS NULL OR leased_until <= $1))
		FROM crawler_frontier`, f.now().UTC(),
	).Scan(&total, &ready, &leasedCount, &nextReady)
	if err != nil {
		return 0, false, fmt.Errorf("crawler: frontier pending: %w", err)
	}
	switch {
	case total == 0:
		return 0, false, nil
	case ready > 0 || leasedCount > 0:
		return 0, true, nil
	case nextReady != nil:
		return maxDuration(0, nextReady.Sub(f.now().UTC())), true, nil
	default:
		return 0, true, nil
	}
}

// Reset implements Frontier. With an empty runID it clears everything, which is
// what a fresh crawl wants; with a runID it clears only that run's queue and its
// seen keys, so a concurrent run is untouched.
func (f *PostgresFrontier) Reset(ctx context.Context, runID string) error {
	if runID == "" {
		if _, err := f.pool.Exec(ctx, `DELETE FROM crawler_frontier`); err != nil {
			return fmt.Errorf("crawler: reset frontier: %w", err)
		}
		if _, err := f.pool.Exec(ctx, `DELETE FROM crawler_frontier_seen`); err != nil {
			return fmt.Errorf("crawler: reset seen keys: %w", err)
		}
		if _, err := f.pool.Exec(ctx, `DELETE FROM host_limits`); err != nil {
			return fmt.Errorf("crawler: reset host limits: %w", err)
		}
		return nil
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM crawler_frontier WHERE run_id = $1`, runID); err != nil {
		return fmt.Errorf("crawler: reset run %s: %w", runID, err)
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM crawler_frontier_seen WHERE run_id = $1`, runID); err != nil {
		return fmt.Errorf("crawler: reset run %s seen keys: %w", runID, err)
	}
	return nil
}

// Close implements Frontier. It is a no-op: the pool belongs to the process, and
// several frontiers can share it.
func (f *PostgresFrontier) Close() {}

// sortItems orders highest priority first, then oldest first.
func sortItems(items []Item) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Priority != items[j].Priority {
			return items[i].Priority > items[j].Priority
		}
		return items[i].EnqueuedAt.Before(items[j].EnqueuedAt)
	})
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
