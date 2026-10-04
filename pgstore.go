package crawler

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hmza-hb/crawler/internal/platform/db"
)

// Migrations holds the crawler's schema. Every module owns its own migrations
// and the lead-engine CLI passes them all to db.Migrate together, so a new
// module's schema ships with that module rather than in a central place.
//
//go:embed migrations/*.sql
var Migrations embed.FS

// MigrationSource returns the crawler's migrations for db.Migrate.
func MigrationSource() db.Source {
	sub, err := fs.Sub(Migrations, "migrations")
	if err != nil {
		// Unreachable: the directory is embedded, so a failure here means the
		// binary was built wrong rather than that the environment is bad.
		panic("crawler: embedded migrations are missing: " + err.Error())
	}
	return db.Source{Module: "crawler", FS: sub}
}

// Store errors.
var (
	// ErrNotFound is returned when a document ID is unknown.
	ErrNotFound = errors.New("crawler: document not found")
	// ErrStoreClosed is returned once the store has been closed.
	ErrStoreClosed = errors.New("crawler: store is closed")
)

// PostgresStore is the durable Store. It is what lets a crawl be resumable and
// what makes conditional re-fetch possible across restarts.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore wraps a pool. The pool is not owned by the store: the
// process that built it closes it.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

const insertDocument = `
INSERT INTO crawler_documents (
    url, requested_url, redirect_chain, status, status_text, content_type, charset,
    content_length, etag, last_modified, content_hash, raw_hash, body,
    body_truncated, not_modified, filtered, depth, fetched_at, elapsed_ms, attempt,
    user_agent, source_hint, run_id, links
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13,
    $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24
)
ON CONFLICT (url) DO UPDATE SET
    status           = EXCLUDED.status,
    status_text      = EXCLUDED.status_text,
    content_type     = EXCLUDED.content_type,
    content_length   = EXCLUDED.content_length,
    etag             = EXCLUDED.etag,
    last_modified    = EXCLUDED.last_modified,
    content_hash     = EXCLUDED.content_hash,
    raw_hash         = EXCLUDED.raw_hash,
    body             = EXCLUDED.body,
    body_truncated   = EXCLUDED.body_truncated,
    not_modified     = EXCLUDED.not_modified,
    filtered         = EXCLUDED.filtered,
    depth            = EXCLUDED.depth,
    fetched_at       = EXCLUDED.fetched_at,
    elapsed_ms       = EXCLUDED.elapsed_ms,
    attempt          = EXCLUDED.attempt,
    user_agent       = EXCLUDED.user_agent,
    source_hint      = EXCLUDED.source_hint,
    run_id           = EXCLUDED.run_id,
    links            = EXCLUDED.links
RETURNING id`

// Save implements Store. A repeat fetch of the same URL updates the existing row
// in place, so a daily crawl of a static site does not grow the table.
func (s *PostgresStore) Save(ctx context.Context, doc Document) (Document, error) {
	links, err := json.Marshal(nonNilLinks(doc.Links))
	if err != nil {
		return doc, fmt.Errorf("crawler: encode links: %w", err)
	}
	var (
		body       []byte
		lastModded *time.Time
	)
	if len(doc.Body) > 0 {
		body = doc.Body
	}
	if !doc.LastModified.IsZero() {
		t := doc.LastModified.UTC()
		lastModded = &t
	}
	fetchedAt := doc.FetchedAt
	if fetchedAt.IsZero() {
		fetchedAt = time.Now().UTC()
	}
	chain := doc.RedirectChain
	if chain == nil {
		chain = []string{}
	}

	var id string
	err = s.pool.QueryRow(ctx, insertDocument,
		doc.URL, doc.RequestedURL, chain, doc.Status, doc.StatusText, doc.ContentType, doc.Charset,
		doc.ContentLength, doc.ETag, lastModded, doc.ContentHash, doc.RawHash, body,
		doc.BodyTruncated, doc.NotModified, doc.Filtered, doc.Depth, fetchedAt,
		doc.Elapsed.Milliseconds(), doc.Attempt, doc.UserAgent, doc.SourceHint,
		doc.RunID, links,
	).Scan(&id)
	if err != nil {
		return doc, fmt.Errorf("crawler: save document %s: %w", doc.URL, err)
	}
	doc.ID = id
	return doc, nil
}

const selectDocumentColumns = `id, url, requested_url, redirect_chain, status, status_text,
	content_type, charset, content_length, etag, last_modified, content_hash, raw_hash, body,
	body_truncated, not_modified, filtered, depth, fetched_at, elapsed_ms, attempt, user_agent,
	source_hint, run_id, links`

// LastDocument implements Store.
func (s *PostgresStore) LastDocument(ctx context.Context, url string) (*Document, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+selectDocumentColumns+` FROM crawler_documents WHERE url = $1`, url)
	doc, err := scanDocument(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("crawler: last document %s: %w", url, err)
	}
	return &doc, nil
}

// Get implements Store.
func (s *PostgresStore) Get(ctx context.Context, id string) (*Document, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+selectDocumentColumns+` FROM crawler_documents WHERE id = $1`, id)
	doc, err := scanDocument(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("crawler: get document %s: %w", id, err)
	}
	return &doc, nil
}

// Seen implements Store.
func (s *PostgresStore) Seen(ctx context.Context, contentHash string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM crawler_documents WHERE content_hash = $1)`, contentHash).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("crawler: seen %s: %w", contentHash, err)
	}
	return ok, nil
}

// Stats implements Store.
func (s *PostgresStore) Stats(ctx context.Context, runID string) (Stats, error) {
	var st Stats
	// A run with no rows is a legitimate answer, not an error: a caller may ask
	// about a run before anything has been stored.
	err := s.pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE NOT filtered AND NOT not_modified),
			coalesce(sum(octet_length(body)), 0),
			count(*) FILTER (WHERE not_modified),
			count(*) FILTER (WHERE filtered),
			count(*) FILTER (WHERE status >= 400),
			min(fetched_at), max(fetched_at)
		FROM crawler_documents
		WHERE ($1 = '' OR run_id = $1)`, runID,
	).Scan(&st.Documents, &st.BytesRetained, &st.NotModified, &st.Filtered, &st.Failed, &st.StartedAt, &st.FinishedAt)
	if err != nil {
		return st, fmt.Errorf("crawler: stats: %w", err)
	}
	if st.StartedAt.IsZero() {
		st.StartedAt, st.FinishedAt = time.Time{}, time.Time{}
		return st, nil
	}
	st.DurationMillis = st.FinishedAt.Sub(st.StartedAt).Milliseconds()
	return st, nil
}

// RecordRun writes a completed run's accounting.
func (s *PostgresStore) RecordRun(ctx context.Context, r Result) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO crawler_runs (
			run_id, started_at, finished_at, depth, stop_reason,
			pages_fetched, bytes_retained, skipped, failed, not_modified, filtered, duration_ms
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (run_id) DO UPDATE SET
			finished_at    = EXCLUDED.finished_at,
			stop_reason    = EXCLUDED.stop_reason,
			pages_fetched  = EXCLUDED.pages_fetched,
			bytes_retained = EXCLUDED.bytes_retained,
			skipped        = EXCLUDED.skipped,
			failed         = EXCLUDED.failed,
			not_modified   = EXCLUDED.not_modified,
			filtered       = EXCLUDED.filtered,
			duration_ms    = EXCLUDED.duration_ms`,
		r.RunID, r.Stats.StartedAt, r.Stats.FinishedAt, "", r.Stats.StoppedBy,
		r.Fetched, r.BytesRetained, r.Skipped, r.Failed, r.NotModified, r.Filtered,
		r.Stats.DurationMillis)
	if err != nil {
		return fmt.Errorf("crawler: record run %s: %w", r.RunID, err)
	}
	return nil
}

// Close implements Store. It closes the pool, because the store is the only
// holder of it in a standalone crawler process.
func (s *PostgresStore) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

// Pool exposes the underlying pool for callers that need to run their own
// queries against crawler tables.
func (s *PostgresStore) Pool() *pgxpool.Pool { return s.pool }

type rowScanner interface {
	Scan(dest ...any) error
}

func scanDocument(row rowScanner) (Document, error) {
	var (
		doc        Document
		chain      []string
		lastModded *time.Time
		body       []byte
		linksJSON  []byte
		elapsedMS  int64
		runID      string
	)
	err := row.Scan(
		&doc.ID, &doc.URL, &doc.RequestedURL, &chain, &doc.Status, &doc.StatusText,
		&doc.ContentType, &doc.Charset, &doc.ContentLength, &doc.ETag, &lastModded,
		&doc.ContentHash, &doc.RawHash, &body, &doc.BodyTruncated, &doc.NotModified,
		&doc.Filtered, &doc.Depth, &doc.FetchedAt, &elapsedMS, &doc.Attempt,
		&doc.UserAgent, &doc.SourceHint, &runID, &linksJSON,
	)
	if err != nil {
		return doc, err
	}
	doc.RedirectChain = chain
	doc.Body = body
	doc.Elapsed = time.Duration(elapsedMS) * time.Millisecond
	doc.FetchedAt = doc.FetchedAt.UTC()
	if lastModded != nil {
		doc.LastModified = lastModded.UTC()
	}
	if len(linksJSON) > 0 {
		if err := json.Unmarshal(linksJSON, &doc.Links); err != nil {
			// A corrupt links column must not hide an otherwise good document.
			doc.Links = nil
		}
	}
	return doc, nil
}

func nonNilLinks(links []Link) []Link {
	if links == nil {
		return []Link{}
	}
	return links
}
