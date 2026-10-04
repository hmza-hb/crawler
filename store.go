package crawler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// newID returns a random 128-bit identifier. A random ID rather than a content
// hash keeps two genuinely different documents that happen to share a hash
// addressable, and keeps IDs unguessable.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; a time-seeded fallback keeps
		// the store usable rather than panicking inside a library.
		return hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b[:])
}

// Store persists fetched documents. It is the crawler's only durable state and
// the only thing the Fetcher reads to make a conditional request.
type Store interface {
	// Save writes a document and returns it with ID assigned. A document whose
	// content hash already exists for that URL updates the existing row rather
	// than inserting a duplicate, but still records the new fetch.
	Save(ctx context.Context, doc Document) (Document, error)
	// LastDocument returns the most recent stored document for a URL, or nil
	// when the URL has never been fetched.
	LastDocument(ctx context.Context, url string) (*Document, error)
	// Get returns a stored document by ID.
	Get(ctx context.Context, id string) (*Document, error)
	// Seen reports whether a content hash is already known, which is how a
	// repeated crawl of a static site costs one conditional request per URL
	// rather than one row per URL per day.
	Seen(ctx context.Context, contentHash string) (bool, error)
	// Stats summarises a crawl for the API and for logs.
	Stats(ctx context.Context, runID string) (Stats, error)
	// Close releases resources.
	Close()
}

// Stats summarises a crawl.
type Stats struct {
	RunID          string    `json:"run_id,omitempty"`
	Documents      int       `json:"documents"`
	BytesRetained  int64     `json:"bytes_retained"`
	Skipped        int       `json:"skipped"`
	Failed         int       `json:"failed"`
	NotModified    int       `json:"not_modified"`
	Filtered       int       `json:"filtered"`
	StartedAt      time.Time `json:"started_at,omitempty"`
	FinishedAt     time.Time `json:"finished_at,omitempty"`
	DurationMillis int64     `json:"duration_ms"`
	StoppedBy      string    `json:"stopped_by,omitempty"`
}

// MemoryStore keeps documents in process memory. It backs tests and short
// one-shot runs; nothing survives a restart.
//
// It is safe for concurrent use: a Crawler's workers save in parallel, and an
// unguarded map write here is a real crash under load, not a theoretical one.
type MemoryStore struct {
	mu     sync.RWMutex
	docs   map[string]Document // by ID
	byURL  map[string]string   // URL -> ID
	byHash map[string]string   // content hash -> ID
	order  []string            // insertion order
	now    func() time.Time
	closed bool
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		docs:   map[string]Document{},
		byURL:  map[string]string{},
		byHash: map[string]string{},
		now:    time.Now,
	}
}

// SetClock replaces the time source, for deterministic tests.
func (s *MemoryStore) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// Save implements Store.
func (s *MemoryStore) Save(_ context.Context, doc Document) (Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return doc, ErrStoreClosed
	}
	// A repeated fetch of the same bytes is a refresh, not a new document.
	if doc.ContentHash != "" {
		if id, ok := s.byHash[doc.ContentHash]; ok {
			if existing, ok := s.docs[id]; ok {
				existing.FetchedAt = doc.FetchedAt
				existing.Attempt = doc.Attempt
				existing.Elapsed = doc.Elapsed
				existing.Status = doc.Status
				s.docs[id] = existing
				return existing, nil
			}
		}
	}
	if doc.FetchedAt.IsZero() {
		doc.FetchedAt = s.now().UTC()
	}
	if doc.ID == "" {
		doc.ID = newID()
	}
	s.docs[doc.ID] = doc
	if doc.URL != "" {
		if old, ok := s.byURL[doc.URL]; ok {
			// Replacing in place keeps LastDocument correct without a second
			// index scan.
			delete(s.docs, old)
		}
		s.byURL[doc.URL] = doc.ID
		s.order = append(s.order, doc.ID)
	}
	if doc.ContentHash != "" {
		s.byHash[doc.ContentHash] = doc.ID
	}
	return doc, nil
}

// LastDocument implements Store.
func (s *MemoryStore) LastDocument(_ context.Context, url string) (*Document, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrStoreClosed
	}
	id, ok := s.byURL[url]
	if !ok {
		return nil, nil
	}
	doc, ok := s.docs[id]
	if !ok {
		return nil, nil
	}
	clone := doc
	return &clone, nil
}

// Get implements Store.
func (s *MemoryStore) Get(_ context.Context, id string) (*Document, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	doc, ok := s.docs[id]
	if !ok {
		return nil, ErrNotFound
	}
	clone := doc
	return &clone, nil
}

// Seen implements Store.
func (s *MemoryStore) Seen(_ context.Context, contentHash string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.byHash[contentHash]
	return ok, nil
}

// Stats implements Store.
func (s *MemoryStore) Stats(context.Context, string) (Stats, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Stats{Documents: len(s.docs)}
	for _, d := range s.docs {
		st.BytesRetained += int64(len(d.Body))
		switch {
		case d.NotModified:
			st.NotModified++
		case d.Filtered:
			st.Filtered++
		case d.Status >= 400:
			st.Failed++
		}
	}
	return st, nil
}

// Close implements Store.
func (s *MemoryStore) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}

// All returns every stored document, in insertion order. Tests use it to assert
// what a crawl actually produced; it is not part of Store.
func (s *MemoryStore) All() []Document {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Document, 0, len(s.order))
	for _, id := range s.order {
		if d, ok := s.docs[id]; ok {
			out = append(out, d)
		}
	}
	return out
}
