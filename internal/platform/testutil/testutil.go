package testutil

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func Postgres(t testing.TB) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres test")
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL: %v", err)
	}

	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping TEST_DATABASE_URL: %v", err)
	}

	schemaName := fmt.Sprintf("test_schema_%d", time.Now().UnixNano())
	_, err = pool.Exec(ctx, fmt.Sprintf("CREATE SCHEMA %s; SET search_path TO %s, public;", schemaName, schemaName))
	if err != nil {
		t.Fatalf("create test schema: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = pool.Exec(cleanupCtx, fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE;", schemaName))
		pool.Close()
	})

	return pool
}
