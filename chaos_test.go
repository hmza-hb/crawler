package crawler_test

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hmza-hb/crawler"
)

// TestChaosMultiWorkerFrontier runs concurrent claimers that randomly abandon
// leases (simulating killed workers). Every enqueued URL must be completed and
// the queue must drain.
func TestChaosMultiWorkerFrontier(t *testing.T) {
	pool := newPool(t)
	frontier := crawler.NewPostgresFrontier(pool)
	defer frontier.Close()
	ctx := context.Background()
	runID := fmt.Sprintf("chaos-%d", time.Now().UnixNano())
	if err := frontier.Reset(ctx, runID); err != nil {
		t.Fatal(err)
	}

	const (
		urlCount = 24
		workers  = 4
	)
	items := make([]crawler.Item, 0, urlCount)
	for i := 0; i < urlCount; i++ {
		key := fmt.Sprintf("url-%d", i)
		items = append(items, crawler.Item{
			URL:    "https://example.com/" + key,
			Key:    key,
			RunID:  runID,
			Depth:  0,
			Priority: 1,
		})
	}
	if _, err := frontier.Enqueue(ctx, items); err != nil {
		t.Fatal(err)
	}

	var completed atomic.Int32
	done := make(chan struct{})
	var once sync.Once
	closeDone := func() { once.Do(func() { close(done) }) }

	lease := 30 * time.Millisecond

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(worker + 1)))
			for {
				select {
				case <-done:
					return
				case <-ctx.Done():
					return
				default:
				}
				batch, err := frontier.Claim(ctx, 3, lease)
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if len(batch) == 0 {
					depth, derr := frontier.Depth(ctx)
					if derr != nil {
						t.Errorf("depth: %v", derr)
						return
					}
					if depth == 0 {
						closeDone()
						return
					}
					time.Sleep(5 * time.Millisecond)
					continue
				}
				for _, it := range batch {
					if rng.Float64() < 0.25 {
						// Simulate a worker killed before Complete: lease expires.
						time.Sleep(lease + 5*time.Millisecond)
						continue
					}
					if err := frontier.Complete(ctx, []crawler.Item{it}, nil); err != nil {
						t.Errorf("complete: %v", err)
						return
					}
					completed.Add(1)
				}
			}
		}(w)
	}

	deadline := time.After(45 * time.Second)
	select {
	case <-done:
	case <-deadline:
		closeDone()
		wg.Wait()
		t.Fatal("chaos test timed out before the frontier drained")
	}
	wg.Wait()

	if n := int(completed.Load()); n < urlCount {
		t.Errorf("completed = %d, want at least %d (reclaim may complete some URLs more than once)", n, urlCount)
	}
	depth, err := frontier.Depth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if depth != 0 {
		t.Errorf("frontier depth = %d, want 0", depth)
	}
}
