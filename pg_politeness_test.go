package crawler_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hmza-hb/crawler"
)

func TestPostgresFrontierSharedHostPoliteness(t *testing.T) {
	pool := newPool(t)
	frontier := crawler.NewPostgresFrontier(pool)
	defer frontier.Close()
	const delay = 80 * time.Millisecond
	frontier.SetPerHostDelay(delay)

	ctx := context.Background()
	runID := fmt.Sprintf("polite-%d", time.Now().UnixNano())
	if err := frontier.Reset(ctx, ""); err != nil {
		t.Fatal(err)
	}

	const n = 12
	items := make([]crawler.Item, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, crawler.Item{
			URL:    fmt.Sprintf("https://example.com/p/%d", i),
			Key:    fmt.Sprintf("k%d", i),
			RunID:  runID,
			Priority: 1,
		})
	}
	otherRun := fmt.Sprintf("%s-other", runID)
	items = append(items, crawler.Item{
		URL:   "https://other.test/x",
		Key:   "other",
		RunID: otherRun,
	})

	if _, err := frontier.Enqueue(ctx, items); err != nil {
		t.Fatal(err)
	}

	var (
		mu       sync.Mutex
		claimAt  []time.Time
		otherAt  []time.Time
		wg       sync.WaitGroup
		workers  = 1 // gap timing is per-domain; multiple workers race host_limits
		stop     = make(chan struct{})
	)

	worker := func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			batch, err := frontier.Claim(ctx, 2, time.Minute)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if len(batch) == 0 {
				depth, _ := frontier.Depth(ctx)
				if depth == 0 {
					return
				}
				time.Sleep(5 * time.Millisecond)
				continue
			}
			now := time.Now()
			for _, it := range batch {
				mu.Lock()
				if it.RunID == otherRun {
					otherAt = append(otherAt, now)
				} else {
					claimAt = append(claimAt, now)
				}
				mu.Unlock()
				_ = frontier.Complete(ctx, []crawler.Item{it}, nil)
			}
		}
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go worker()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		close(stop)
		t.Fatal("politeness test timed out")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(claimAt) < 3 {
		t.Fatalf("claims = %d, want several for example.com", len(claimAt))
	}
	for i := 1; i < len(claimAt); i++ {
		gap := claimAt[i].Sub(claimAt[i-1])
		if gap < delay-time.Millisecond*15 {
			t.Errorf("gap[%d] = %v, want >= %v (shared host_limits)", i, gap, delay)
		}
	}
	if len(otherAt) == 0 {
		t.Fatal("expected parallel claim on other.test")
	}
}
