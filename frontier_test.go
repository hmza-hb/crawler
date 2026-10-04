package crawler

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func item(key string, priority, depth int) Item {
	return Item{URL: "https://example.com/" + key, Key: key, Priority: priority, Depth: depth}
}

func TestMemoryFrontierDeduplicatesByKey(t *testing.T) {
	f := NewMemoryFrontier(10)
	added, err := f.Enqueue(context.Background(), []Item{item("a", 0, 0), item("a", 0, 0), item("b", 0, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if added != 2 {
		t.Errorf("added = %d, want 2 (the duplicate must be dropped)", added)
	}
	if n, _ := f.Depth(context.Background()); n != 2 {
		t.Errorf("depth = %d, want 2", n)
	}
}

func TestMemoryFrontierClaimsHighestPriorityFirst(t *testing.T) {
	f := NewMemoryFrontier(10)
	_, _ = f.Enqueue(context.Background(), []Item{
		item("low", 1, 0), item("high", 9, 0), item("mid", 5, 0),
	})

	got, err := f.Claim(context.Background(), 3, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("claimed %d, want 3", len(got))
	}
	if got[0].Key != "high" || got[1].Key != "mid" || got[2].Key != "low" {
		t.Errorf("claim order = %v, want high, mid, low", keys(got))
	}
}

func TestMemoryFrontierDoesNotDoubleClaimWithinLease(t *testing.T) {
	f := NewMemoryFrontier(10)
	_, _ = f.Enqueue(context.Background(), []Item{item("a", 1, 0)})

	first, _ := f.Claim(context.Background(), 5, time.Minute)
	if len(first) != 1 {
		t.Fatalf("first claim = %d items, want 1", len(first))
	}
	second, _ := f.Claim(context.Background(), 5, time.Minute)
	if len(second) != 0 {
		t.Errorf("second claim = %d items, want 0 while the lease holds", len(second))
	}
}

func TestMemoryFrontierReclaimsExpiredLease(t *testing.T) {
	now := time.Now()
	f := NewMemoryFrontier(10)
	f.SetClock(func() time.Time { return now })
	_, _ = f.Enqueue(context.Background(), []Item{item("a", 1, 0)})

	if got, _ := f.Claim(context.Background(), 1, time.Second); len(got) != 1 {
		t.Fatalf("first claim = %d, want 1", len(got))
	}
	// A worker that dies mid-fetch must not strand its item forever.
	now = now.Add(2 * time.Second)
	got, _ := f.Claim(context.Background(), 1, time.Second)
	if len(got) != 1 {
		t.Errorf("reclaim after lease expiry = %d, want 1", len(got))
	}
	if got[0].Attempts != 2 {
		t.Errorf("attempts = %d, want 2", got[0].Attempts)
	}
}

func TestMemoryFrontierCompleteDrainsItem(t *testing.T) {
	f := NewMemoryFrontier(10)
	_, _ = f.Enqueue(context.Background(), []Item{item("a", 1, 0)})
	claimed, _ := f.Claim(context.Background(), 1, time.Minute)
	if err := f.Complete(context.Background(), claimed, nil); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.Depth(context.Background()); n != 0 {
		t.Errorf("depth = %d, want 0 after completion", n)
	}
}

func TestMemoryFrontierReleaseRetriesThenDrops(t *testing.T) {
	now := time.Now()
	f := NewMemoryFrontier(10)
	f.SetClock(func() time.Time { return now })
	_, _ = f.Enqueue(context.Background(), []Item{item("a", 1, 0)})

	// A transient failure is released, and the backoff keeps it out of the next
	// claim until enough time has passed.
	claimed, _ := f.Claim(context.Background(), 1, time.Second)
	_ = f.Complete(context.Background(), claimed, errRelease)
	if got, _ := f.Claim(context.Background(), 1, time.Second); len(got) != 0 {
		t.Errorf("claim during backoff = %d, want 0", len(got))
	}

	now = now.Add(2 * time.Minute)
	for i := 0; i < 3; i++ {
		claimed, _ := f.Claim(context.Background(), 1, time.Second)
		if len(claimed) == 0 {
			break
		}
		_ = f.Complete(context.Background(), claimed, errRelease)
		now = now.Add(2 * time.Minute)
	}
	// After repeated failure the item is dropped rather than retried forever.
	if got, _ := f.Claim(context.Background(), 1, time.Second); len(got) != 0 {
		t.Errorf("a permanently failing item = %d claims, want it dropped", len(got))
	}
	if n, _ := f.Depth(context.Background()); n != 0 {
		t.Errorf("depth = %d, want 0", n)
	}
}

func TestMemoryFrontierBoundsSize(t *testing.T) {
	f := NewMemoryFrontier(3)
	batch := make([]Item, 0, 10)
	for i := 0; i < 10; i++ {
		batch = append(batch, Item{URL: fmt.Sprintf("https://example.com/%d", i), Key: fmt.Sprintf("k%d", i), Priority: i})
	}
	added, err := f.Enqueue(context.Background(), batch)
	if err != nil {
		t.Fatal(err)
	}
	// Every item is accepted, but the queue stays bounded by evicting the
	// lowest-priority waiter, so the highest-priority items survive.
	if added != 10 {
		t.Errorf("added = %d, want all 10 accepted", added)
	}
	if n, _ := f.Depth(context.Background()); n != 3 {
		t.Errorf("depth = %d, want the 3-item cap", n)
	}
	if f.Evictions() != 7 {
		t.Errorf("evictions = %d, want 7", f.Evictions())
	}
	got, _ := f.Claim(context.Background(), 3, time.Minute)
	if keys(got)[0] != "k9" {
		t.Errorf("highest priority item was evicted: claimed %v", keys(got))
	}
}

func TestMemoryFrontierClosed(t *testing.T) {
	f := NewMemoryFrontier(10)
	f.Close()
	if _, err := f.Enqueue(context.Background(), []Item{item("a", 1, 0)}); err == nil {
		t.Error("Enqueue after Close must fail")
	}
	if _, err := f.Claim(context.Background(), 1, time.Minute); err == nil {
		t.Error("Claim after Close must fail")
	}
}

func TestFrontierKeyIgnoresTrackingParameters(t *testing.T) {
	a := frontierKeyFor("https://example.com/p?utm_source=x&id=7")
	b := frontierKeyFor("https://example.com/p?id=7")
	if a == "" || a != b {
		t.Errorf("keys %q and %q must be equal; tracking parameters cannot change identity", a, b)
	}
	if frontierKeyFor("://not a url") != "" {
		t.Error("an unparseable URL must yield an empty key")
	}
}

func TestRunStateBudgets(t *testing.T) {
	state := &RunState{StartedAt: time.Now().Add(-time.Hour), PerHost: map[string]int{"a.com": 5}}
	b := Budget{MaxPages: 2, MaxHostPages: 5, MaxDuration: time.Minute}
	now := time.Now()

	state.PagesFetched = 2
	if reason, spent := b.exhausted(state, now); !spent || reason != "max_pages" {
		t.Errorf("exhausted = (%q, %v), want (max_pages, true)", reason, spent)
	}

	state.PagesFetched = 0
	if reason, spent := b.exhausted(state, now); !spent || reason != "max_duration" {
		t.Errorf("exhausted = (%q, %v), want (max_duration, true)", reason, spent)
	}

	state.StartedAt = now
	if _, spent := b.exhausted(state, now); spent {
		t.Error("a fresh run must not report a spent budget")
	}
}

func TestCrawlerClaimHostSlotIsABoundNotASuggestion(t *testing.T) {
	c := &Crawler{state: RunState{PerHost: map[string]int{}}}

	for i := 0; i < 2; i++ {
		if !c.claimHostSlot("https://a.com/page", 2) {
			t.Fatalf("claim %d was refused, want the first 2 to succeed", i+1)
		}
	}
	if c.claimHostSlot("https://a.com/page", 2) {
		t.Error("the third claim from the same host must be refused")
	}
	if !c.claimHostSlot("https://b.com/page", 2) {
		t.Error("a different host must have its own allowance")
	}
	if !c.claimHostSlot("https://a.com/page", 0) {
		t.Error("a zero limit must mean unlimited")
	}
}

func TestMemoryStoreRoundTrip(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()

	doc := Document{URL: "https://example.com/", ContentHash: "h1", Body: []byte("hi"), Status: 200}
	saved, err := s.Save(ctx, doc)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID == "" {
		t.Fatal("Save must assign an ID")
	}
	got, err := s.Get(ctx, saved.ID)
	if err != nil || got == nil {
		t.Fatalf("Get = (%v, %v), want the saved document", got, err)
	}
	if string(got.Body) != "hi" {
		t.Errorf("body = %q, want hi", got.Body)
	}
	last, _ := s.LastDocument(ctx, "https://example.com/")
	if last == nil || last.ID != saved.ID {
		t.Error("LastDocument must return the saved document")
	}
	seen, _ := s.Seen(ctx, "h1")
	if !seen {
		t.Error("Seen must report a known content hash")
	}
	if _, err := s.Get(ctx, "nope"); err != ErrNotFound {
		t.Errorf("Get(unknown) = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreRefreshesOnIdenticalContent(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()

	first, _ := s.Save(ctx, Document{URL: "https://e.com/", ContentHash: "same", FetchedAt: time.Unix(1, 0)})
	second, _ := s.Save(ctx, Document{URL: "https://e.com/", ContentHash: "same", FetchedAt: time.Unix(2, 0)})
	if first.ID != second.ID {
		t.Errorf("re-saving identical content created a new row (%s then %s); a daily crawl must not grow the table", first.ID, second.ID)
	}
	if len(s.All()) != 1 {
		t.Errorf("stored %d documents, want 1", len(s.All()))
	}
}

func keys(items []Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Key
	}
	return out
}
