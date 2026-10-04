package crawler

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresFrontier is a durable, shareable Frontier. Several crawler processes
// can claim from the same queue, and a process that dies mid-fetch leaves a
// lease that expires rather than a permanently stuck item.
type PostgresFrontier struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewPostgresFrontier wraps a pool.
func NewPostgresFrontier(pool *pgxpool.Pool) *PostgresFrontier {
	return &PostgresFrontier{pool: pool, now: time.Now}
}

// SetClock replaces the time source, for deterministic tests.
func (f *PostgresFrontier) SetClock(now func() time.Time) { f.now = now }

// Enqueue implements Frontier.
//
// The dedupe gate is a single statement: a CTE inserts the run's key into
// crawler_frontier_seen and queues the URL only if that insert was new. Doing it
// as two statements would require deleting the queue row on a conflict, and that
// delete would also remove a legitimately queued item for the same key.
func (f *PostgresFrontier) Enqueue(ctx context.Context, items []Item) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}
	batch := &pgx.Batch{}
	now := f.now().UTC()
	for _, it := range items {
		batch.Queue(`
			WITH seen AS (
				INSERT INTO crawler_frontier_seen (run_id, dedupe_key, seen_at)
				VALUES ($1, $2, $3)
				ON CONFLICT (run_id, dedupe_key) DO NOTHING
				RETURNING 1
			)
			INSERT INTO crawler_frontier (
				dedupe_key, url, depth, priority, source_hint, run_id, enqueued_at, ready_at
			)
			SELECT $2, $4, $5, $6, $7, $1, $3, $3
			FROM seen`,
			it.RunID, itemKey(it), now, it.URL, it.Depth, it.Priority, it.SourceHint)
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
	rows, err := f.pool.Query(ctx, `
		WITH claimable AS (
			SELECT id FROM crawler_frontier
			WHERE ready_at <= $1
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
	// UPDATE ... RETURNING does not preserve the CTE's ORDER BY, so the priority
	// order has to be restored here. A frontier that hands out work in arbitrary
	// order is a frontier that spends a deep budget on a shallow page.
	sortItems(out)
	return out, nil
}

// sortItems orders highest priority first, then oldest first.
func sortItems(items []Item) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Priority != items[j].Priority {
			return items[i].Priority > items[j].Priority
		}
		return items[i].EnqueuedAt.Before(items[j].EnqueuedAt)
	})
}

// Complete implements Frontier. A nil error deletes the item; an error backs it
// off exponentially, and an item that keeps failing is dropped so a crawl is
// not a queue with infinite patience.
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
		if _, updErr := f.pool.Exec(ctx, `
			UPDATE crawler_frontier
			SET failures    = failures + 1,
			    leased_until = NULL,
			    ready_at    = $3 + make_interval(secs => LEAST(300, (failures + 1) * 2))
			WHERE run_id = $1 AND dedupe_key = $2`,
			it.RunID, itemKey(it), now); updErr != nil {
			return fmt.Errorf("crawler: release %s: %w", it.URL, updErr)
		}
		if _, delErr := f.pool.Exec(ctx, `
			DELETE FROM crawler_frontier
			WHERE run_id = $1 AND dedupe_key = $2 AND failures >= 3`,
			it.RunID, itemKey(it)); delErr != nil {
			return fmt.Errorf("crawler: drop %s: %w", it.URL, delErr)
		}
	}
	return nil
}

// Depth implements Frontier.
func (f *PostgresFrontier) Depth(ctx context.Context) (int, error) {
	var n int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM crawler_frontier`).Scan(&n); err != nil {
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
		SELECT count(*),
		       count(*) FILTER (WHERE ready_at <= $1 AND (leased_until IS NULL OR leased_until <= $1)),
		       count(*) FILTER (WHERE leased_until > $1),
		       min(ready_at) FILTER (WHERE ready_at > $1 AND (leased_until IS NULL OR leased_until <= $1))
		FROM crawler_frontier`, f.now().UTC(),
	).Scan(&total, &ready, &leasedCount, &nextReady)
	if err != nil {
		return 0, false, fmt.Errorf("crawler: frontier pending: %w", err)
	}
	switch {
	case total == 0:
		return 0, false, nil
	case ready > 0 || leasedCount > 0:
		// Something is claimable now, or a worker holds it and will signal.
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

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
