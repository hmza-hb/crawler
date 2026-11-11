package crawler_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hmza-hb/crawler"
)

func TestPostgresFrontierFailedState(t *testing.T) {
	pool := newPool(t)
	frontier := crawler.NewPostgresFrontier(pool)
	defer frontier.Close()
	ctx := context.Background()
	runID := fmt.Sprintf("fail-%d", time.Now().UnixNano())
	if err := frontier.Reset(ctx, runID); err != nil {
		t.Fatal(err)
	}

	it := crawler.Item{URL: "https://example.com/bad", Key: "bad", RunID: runID}
	if _, err := frontier.Enqueue(ctx, []crawler.Item{it}); err != nil {
		t.Fatal(err)
	}

	fail := errors.New("permanent upstream error")
	for range 3 {
		claimed, err := frontier.Claim(ctx, 1, time.Minute)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim: %v n=%d", err, len(claimed))
		}
		if err := frontier.Complete(ctx, claimed, fail); err != nil {
			t.Fatal(err)
		}
	}

	n, err := frontier.FailedCount(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("failed count = %d, want 1", n)
	}

	var state, lastErr string
	var failures int
	if err := pool.QueryRow(ctx, `
		SELECT state, last_error, failures FROM crawler_frontier WHERE run_id = $1 AND dedupe_key = $2`,
		runID, "bad").Scan(&state, &lastErr, &failures); err != nil {
		t.Fatal(err)
	}
	if state != "failed" {
		t.Errorf("state = %q, want failed", state)
	}
	if failures < 3 {
		t.Errorf("failures = %d, want >= 3", failures)
	}
	if lastErr == "" {
		t.Error("last_error should be set")
	}

	claimed, _ := frontier.Claim(ctx, 5, time.Second)
	if len(claimed) != 0 {
		t.Errorf("failed row was reclaimable: %d items", len(claimed))
	}
}
