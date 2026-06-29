// Package pgstore provides PostgreSQL persistence for GatewAI usage events.
// The gateway binary writes events asynchronously; the UI binary reads them.
// When no DSN is configured both sides use a no-op path — no PostgreSQL required.
package pgstore

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store wraps a pgxpool connection pool.
type Store struct {
	pool *pgxpool.Pool
}

// New opens a connection pool, runs pending schema migrations, and returns a Store.
// Returns (nil, nil) when dsn is empty — the gateway/UI run without PostgreSQL.
func New(ctx context.Context, dsn string, maxConns int, connectTimeout string) (*Store, error) {
	if dsn == "" {
		return nil, nil
	}

	timeout := 5 * time.Second
	if d, err := time.ParseDuration(connectTimeout); err == nil && d > 0 {
		timeout = d
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("pgstore: parse dsn: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = int32(maxConns)
	}
	cfg.ConnConfig.ConnectTimeout = timeout

	connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(connectCtx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pgstore: connect: %w", err)
	}

	s := &Store{pool: pool}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgstore: migrate: %w", err)
	}
	return s, nil
}

// Close releases all connections in the pool.
func (s *Store) Close() {
	if s != nil {
		s.pool.Close()
	}
}

// migrate reads migration SQL files from the embedded FS and applies any that
// have not yet been recorded in schema_migrations.
func (s *Store) migrate(ctx context.Context) error {
	// Ensure schema_migrations table exists first (bootstrapping).
	_, err := s.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		sql, err := migrationsFS.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}
		if _, err := s.pool.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("apply migration %s: %w", entry.Name(), err)
		}
	}
	return nil
}
