package store

import (
	"context"
	"embed"
	"fmt"
	"strings"
)

//go:embed migrations/*.sql
var migrations embed.FS

func (s *Store) Migrate(ctx context.Context) error {
	for _, name := range []string{"migrations/001_init.sql", "migrations/002_index_runs_pending.sql"} {
		sql, err := migrations.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read store migration: %w", err)
		}
		for _, statement := range strings.Split(string(sql), ";") {
			if statement = strings.TrimSpace(statement); statement == "" {
				continue
			}
			if err := s.conn.Exec(ctx, statement); err != nil {
				return fmt.Errorf("apply store migration: %w", err)
			}
		}
	}
	return nil
}
