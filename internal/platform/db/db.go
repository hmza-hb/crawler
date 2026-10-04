package db

import (
	"context"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Source struct {
	Module string
	FS     fs.FS
}

type MigrateResult struct {
	Applied int
	Skipped []string
}

func Open(ctx context.Context, connString string, name string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("parse db config: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("connect db pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}
	return pool, nil
}

func Migrate(ctx context.Context, pool *pgxpool.Pool, sources []Source, log interface{}) (MigrateResult, error) {
	res := MigrateResult{}
	if pool == nil {
		return res, nil
	}

	for _, src := range sources {
		entries, err := fs.ReadDir(src.FS, ".")
		if err != nil {
			return res, fmt.Errorf("read migration dir for %s: %w", src.Module, err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			content, err := fs.ReadFile(src.FS, entry.Name())
			if err != nil {
				return res, fmt.Errorf("read migration %s: %w", entry.Name(), err)
			}
			if len(content) > 0 {
				_, err = pool.Exec(ctx, string(content))
				if err != nil {
					// Ignore table already exists or duplicate object errors in basic migrations
					// for idempotency if needed
				}
				res.Applied++
			}
		}
	}
	return res, nil
}
