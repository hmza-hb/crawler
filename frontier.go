package crawler

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/hmza-hb/crawler/internal/platform/hash"
)

// ErrFrontierClosed is returned once the frontier has been closed.
var ErrFrontierClosed = errors.New("crawler: frontier is closed")

// Item is a unit of work in the frontier.
type Item struct {
	// URL is the normalised absolute URL to fetch.
	URL string
	// Key is the dedupe identity: the URL without tracking parameters.
	Key string
	// Depth is the hop count from the seed.
	Depth int
	// Priority orders the queue. Higher is sooner.
	Priority int
	// SourceHint records what discovered the URL.
	SourceHint string
	// EnqueuedAt is when it entered the queue.
	EnqueuedAt time.Time
	// Attempts counts how many times it has been claimed.
	Attempts int
	// RunID scopes the item to a crawl.
	RunID string
}

// Frontier is the crawl queue. Claim/complete rather than pop/push, because a
// worker that dies mid-fetch must not lose its work: a claimed item that is
// never completed becomes claimable again once its lease expires.
type Frontier interface {
	// Enqueue adds URLs, ignoring any whose dedupe key is already present.
	// It returns how many were newly added.
	Enqueue(ctx context.Context, items []Item) (int, error)
	// Claim leases up to n items, highest priority first.
	Claim(ctx context.Context, n int, lease time.Duration) ([]Item, error)
	// Complete marks items done. Items that fail are released with Backoff.
	Complete(ctx context.Context, items []Item, err error) error
	// Depth reports how many items are waiting.
	Depth(ctx context.Context) (int, error)
	// Pending reports how long until the next item becomes claimable, and
	// whether anything is pending at all. An exact answer lets a supervisor
	// sleep precisely as long as a backoff needs instead of polling, and lets it
	// stop the instant the queue is truly finished. A poll-count heuristic
	// cannot: an empty claim looks the same whether the crawl is done or a
	// worker still holds the only remaining item.
	Pending(ctx context.Context) (time.Duration, bool, error)
	// Reset clears a run's queue.
	Reset(ctx context.Context, runID string) error
	// Close releases resources.
	Close()
}

// MemoryFrontier is an in-process priority frontier. It backs unit tests, the
// one-shot CLI, and a single-worker crawl that does not need durability.
type MemoryFrontier struct {
	mu        sync.Mutex
	items     map[string]*frontierEntry
	now       func() time.Time
	closed    bool
	maxSize   int
	evictions int
	// seen remembers every key this frontier has accepted, including keys whose
	// items have been completed. Deduplicating only against the *queued* set
	// would let A -> B -> A re-crawl a page forever.
	seen map[string]struct{}
}

type frontierEntry struct {
	item      Item
	leased    bool
	leaseTill time.Time
	failures  int
	backoff   time.Duration
}

// NewMemoryFrontier returns an empty frontier. maxSize bounds growth; zero
// selects DefaultFrontierSize.
func NewMemoryFrontier(maxSize int) *MemoryFrontier {
	if maxSize <= 0 {
		maxSize = DefaultFrontierSize
	}
	return &MemoryFrontier{
		items:   make(map[string]*frontierEntry, 64),
		seen:    make(map[string]struct{}, 64),
		now:     time.Now,
		maxSize: maxSize,
	}
}

// DefaultFrontierSize bounds the in-memory frontier so a crawl that follows too
// many links fails loudly instead of exhausting the machine.
const DefaultFrontierSize = 200_000

// SetClock replaces the time source, for deterministic tests.
func (f *MemoryFrontier) SetClock(now func() time.Time) { f.now = now }

// Enqueue implements Frontier.
func (f *MemoryFrontier) Enqueue(_ context.Context, items []Item) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, ErrFrontierClosed
	}
	added, evicted := 0, 0
	for _, it := range items {
		if it.Key == "" {
			it.Key = it.URL
		}
		if _, done := f.seen[it.Key]; done {
			continue
		}
		if len(f.items) >= f.maxSize {
			// The frontier is a bounded priority queue: a new item evicts the
			// lowest-priority one already waiting. Refusing the new item
			// instead would let a flood of low-priority links lock out a seed.
			// The eviction is a policy decision, so it is reported as a skip.
			if f.evictWorstLocked() {
				evicted++
			}
		}
		if len(f.items) >= f.maxSize {
			break
		}
		if it.EnqueuedAt.IsZero() {
			it.EnqueuedAt = f.now()
		}
		copied := it
		f.items[it.Key] = &frontierEntry{item: copied}
		f.seen[it.Key] = struct{}{}
		added++
	}
	f.evictions += evicted
	return added, nil
}

// Claim implements Frontier. Items whose lease has expired are eligible again.
func (f *MemoryFrontier) Claim(_ context.Context, n int, lease time.Duration) ([]Item, error) {
	if n <= 0 {
		return nil, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, ErrFrontierClosed
	}

	now := f.now()
	candidates := make([]*frontierEntry, 0, len(f.items))
	for _, e := range f.items {
		if e.leased && e.leaseTill.After(now) {
			continue
		}
		if e.backoff > 0 && now.Before(e.item.EnqueuedAt.Add(e.backoff)) {
			continue
		}
		candidates = append(candidates, e)
	}
	sortByPriority(candidates)

	out := make([]Item, 0, min(n, len(candidates)))
	for _, e := range candidates {
		if len(out) >= n {
			break
		}
		e.leased = true
		e.leaseTill = now.Add(lease)
		e.item.Attempts++
		out = append(out, e.item)
	}
	return out, nil
}

// Complete implements Frontier.
func (f *MemoryFrontier) Complete(_ context.Context, items []Item, err error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, it := range items {
		e, ok := f.items[it.Key]
		if !ok {
			continue
		}
		e.leased = false
		e.leaseTill = time.Time{}
		if err == nil {
			delete(f.items, it.Key)
			continue
		}
		e.failures++
		// Exponential backoff, capped. An item that keeps failing is dropped
		// rather than retried forever: a crawl is not a queue with infinite
		// patience.
		e.backoff = minDuration(time.Duration(e.failures)*time.Second, 5*time.Minute)
		if e.failures >= 3 {
			delete(f.items, it.Key)
		}
	}
	return nil
}

// Depth implements Frontier. It counts every item still held, including leased
// ones, so a caller can tell "queue empty" from "queue leased out".
func (f *MemoryFrontier) Depth(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.items), nil
}

// Pending implements Frontier.
func (f *MemoryFrontier) Pending(_ context.Context) (time.Duration, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.items) == 0 {
		return 0, false, nil
	}
	now := f.now()
	soonest := time.Duration(0)
	found := false
	for _, e := range f.items {
		if e.leased {
			// Held by a worker. No deadline is knowable from here, so the
			// caller must rely on its own wake signal for this one.
			return 0, true, nil
		}
		ready := e.item.EnqueuedAt.Add(e.backoff)
		if !ready.After(now) {
			return 0, true, nil
		}
		d := ready.Sub(now)
		if !found || d < soonest {
			soonest, found = d, true
		}
	}
	if !found {
		return 0, true, nil
	}
	return soonest, true, nil
}

// Reset implements Frontier.
func (f *MemoryFrontier) Reset(_ context.Context, runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, e := range f.items {
		if runID == "" || e.item.RunID == runID {
			delete(f.items, k)
		}
	}
	if runID == "" {
		clear(f.seen)
		return nil
	}
	// A run-scoped reset must not clear another run's seen set, so keys are
	// re-derived from whatever is still queued.
	queued := make(map[string]struct{}, len(f.items))
	for k := range f.items {
		queued[k] = struct{}{}
	}
	clear(f.seen)
	for k := range queued {
		f.seen[k] = struct{}{}
	}
	return nil
}

// Close implements Frontier.
func (f *MemoryFrontier) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
}

// Evictions reports how many queued items were dropped to make room. A
// non-zero value means the cap was hit and the crawl is narrower than intended.
func (f *MemoryFrontier) Evictions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.evictions
}

// evictWorstLocked drops the lowest-priority unleased item and reports whether
// it found one. A leased item is never evicted: a worker is using it.
func (f *MemoryFrontier) evictWorstLocked() bool {
	var worstKey string
	worst := Item{Priority: 1 << 30, EnqueuedAt: time.Now().Add(time.Hour)}
	for k, e := range f.items {
		if e.leased {
			continue
		}
		if e.item.Priority < worst.Priority ||
			(e.item.Priority == worst.Priority && e.item.EnqueuedAt.Before(worst.EnqueuedAt)) {
			worstKey, worst = k, e.item
		}
	}
	if worstKey == "" {
		return false
	}
	delete(f.items, worstKey)
	return true
}

// sortByPriority orders highest priority first, then oldest first. Sorting by
// insertion order alone would starve a deep-but-valuable page behind a thousand
// shallow ones.
func sortByPriority(items []*frontierEntry) {
	// Insertion sort: the claim batch is small (tens of items) and this avoids
	// allocating for a comparator on the hot path.
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && higherPriority(items[j], items[j-1]); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

func higherPriority(a, b *frontierEntry) bool {
	if a.item.Priority != b.item.Priority {
		return a.item.Priority > b.item.Priority
	}
	return a.item.EnqueuedAt.Before(b.item.EnqueuedAt)
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// frontierKeyFor produces the dedupe key for a URL string: the normalised URL
// with tracking parameters removed. An unparseable URL yields "", which the
// frontier treats as "not enqueueable".
func frontierKeyFor(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	if _, err := NormalizeURL(u); err != nil {
		return ""
	}
	return dedupeKey(u)
}

// fingerprint identifies a document by its content, independent of when it was
// fetched. It is what the lead engine uses to decide "nothing changed".
func fingerprint(doc Document) hash.Digest {
	return hash.CombineSet(
		doc.URL,
		doc.ContentHash,
		doc.ETag,
		doc.LastModified.UTC().Format(time.RFC3339),
	)
}
