package storage

import (
	"context"
	stdsql "database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"github.com/yarlson/dyne/internal/workflow/storage/sql"
)

// Open connects to SQLite or PostgreSQL and prepares the workflow schema.
func Open(ctx context.Context, databaseURL string) (*Repository, error) {
	driver, dataSource, err := connection(databaseURL)
	if err != nil {
		return nil, err
	}

	if driver == "sqlite" {
		if err := protectSQLiteFile(databaseURL); err != nil {
			return nil, err
		}
	}

	database, err := stdsql.Open(driver, dataSource)
	if err != nil {
		return nil, fmt.Errorf("open workflow database: %w", err)
	}

	if driver == "sqlite" {
		database.SetMaxOpenConns(1)
	}

	if err := database.PingContext(ctx); err != nil {
		_ = database.Close()

		return nil, fmt.Errorf("connect to workflow database: %w", err)
	}

	if err := applySchema(ctx, database, driver); err != nil {
		_ = database.Close()

		return nil, err
	}

	return &Repository{database: database, queries: sql.New(database)}, nil
}

func connection(databaseURL string) (string, string, error) {
	switch {
	case strings.HasPrefix(databaseURL, "sqlite:"):
		location := strings.TrimPrefix(databaseURL, "sqlite:")
		if location == "" {
			return "", "", errors.New("SQLite workflow database path is required")
		}

		if location == ":memory:" {
			return "sqlite", "file:dyne-workflows?mode=memory&cache=shared&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", nil
		}

		dataSource := location
		if !strings.HasPrefix(dataSource, "file:") {
			dataSource = "file:" + dataSource
		}

		separator := "?"
		if strings.Contains(dataSource, "?") {
			separator = "&"
		}

		return "sqlite", dataSource + separator + "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)", nil
	case strings.HasPrefix(databaseURL, "postgres://"), strings.HasPrefix(databaseURL, "postgresql://"):
		return "pgx", databaseURL, nil
	default:
		return "", "", errors.New("unsupported workflow database URL; use sqlite: or postgres://")
	}
}

func protectSQLiteFile(databaseURL string) error {
	location := strings.TrimPrefix(databaseURL, "sqlite:")
	if location == ":memory:" || strings.Contains(location, "mode=memory") {
		return nil
	}

	location = strings.TrimPrefix(location, "file:")
	location, _, _ = strings.Cut(location, "?")
	path, err := url.PathUnescape(location)
	if err != nil {
		return fmt.Errorf("decode SQLite workflow database path: %w", err)
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("create SQLite workflow database: %w", err)
	}

	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()

		return fmt.Errorf("protect SQLite workflow database: %w", err)
	}

	if err := file.Close(); err != nil {
		return fmt.Errorf("close SQLite workflow database: %w", err)
	}

	return nil
}
