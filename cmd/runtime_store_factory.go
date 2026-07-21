package cmd

import (
	"context"
	"database/sql"
	"fmt"

	"xrok/cmd/config"
)

// StoreFactoryDeps keeps database driver selection outside xrok. This allows
// embedders to register the driver they use without imposing dependencies.
type StoreFactoryDeps struct {
	OpenSQL func(driver, dsn string) (*sql.DB, error)
}

func NewRuntimeStateStore(ctx context.Context, cfg config.ServerConfig, deps StoreFactoryDeps) (RuntimeStateStore, error) {
	switch cfg.StateBackend {
	case "", "memory":
		return NewInMemoryStateStore(4096), nil
	case "redis":
		if cfg.RedisAddr == "" {
			return nil, fmt.Errorf("redis_addr is required")
		}
		s := NewRedisStateStore(cfg.RedisAddr)
		s.Password = cfg.RedisPassword
		return s, nil
	case "postgres", "postgresql":
		if cfg.PostgresDSN == "" {
			return nil, fmt.Errorf("postgres_dsn is required")
		}
		if deps.OpenSQL == nil {
			return nil, fmt.Errorf("postgres backend requires OpenSQL dependency")
		}
		db, err := deps.OpenSQL("postgres", cfg.PostgresDSN)
		if err != nil {
			return nil, err
		}
		s := NewPostgresStateStore(db)
		if err := s.EnsureSchema(ctx); err != nil {
			_ = db.Close()
			return nil, err
		}
		return s, nil
	default:
		return nil, fmt.Errorf("unsupported state_backend %q", cfg.StateBackend)
	}
}
