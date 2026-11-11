package crawler_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hmza-hb/crawler"
)

func TestPostgresFrontierSharedHostPoliteness(t *testing.T) {
	pool := newPool(t)
	frontier := crawler.NewPostgresFrontier(pool)
	defer frontier.Close()
	const delay = 100 * time.Millisecond
	frontier.SetPerHostDelay(delay)

	ctx := context.Background()
	runID := fmt.Sprintf("polite-%d", time.Now().UnixNano())
	if err := frontier.Reset(ctx, ""); err != nil {
		t.Fatal(err)
	}

	if _, err := frontier.Enqueue(ctx, []crawler.Item{
		{URL: "https://example.com/a", Key: "a", RunID: runID, Priority: 1},
		{URL: "https://example.com/b", Key: "b", RunID: runID, Priority: 1},
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	frontier.SetClock(func() time.Time { return now })

	first, err := frontier.Claim(ctx, 1, time.Minute)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim: err=%v n=%d", err, len(first))
	}
	if first[0].Key != "a" && first[0].Key != "b" {
		t.Fatalf("first key = %q", first[0].Key)
	}
	if err := frontier.Complete(ctx, first, nil); err != nil {
		t.Fatal(err)
	}

	// Same domain: blocked until delay elapses.
	blocked, err := frontier.Claim(ctx, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked) != 0 {
		t.Fatalf("second claim before delay = %d, want 0", len(blocked))
	}

	now = now.Add(delay)
	second, err := frontier.Claim(ctx, 1, time.Minute)
	if err != nil || len(second) != 1 {
		t.Fatalf("second claim after delay: err=%v n=%d", err, len(second))
	}
	if second[0].Key == first[0].Key {
		t.Fatalf("got same key %q twice", second[0].Key)
	}

	// Other registrable domain is independent of example.com's gate.
	if _, err := frontier.Enqueue(ctx, []crawler.Item{
		{URL: "https://other.test/x", Key: "other", RunID: runID + "-o", Priority: 1},
	}); err != nil {
		t.Fatal(err)
	}
	other, err := frontier.Claim(ctx, 1, time.Minute)
	if err != nil || len(other) != 1 || other[0].Key != "other" {
		t.Fatalf("other domain claim: err=%v got=%+v", err, other)
	}
}
