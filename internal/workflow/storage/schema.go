package storage

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

//go:embed schema/*.sql
var schemaFiles embed.FS

func applySchema(ctx context.Context, database *sql.DB, driver string) error {
	transaction, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin workflow schema update: %w", err)
	}

	defer func() { _ = transaction.Rollback() }()

	if driver == "pgx" {
		if _, err := transaction.ExecContext(ctx, "SELECT pg_advisory_xact_lock(1685677683)"); err != nil {
			return fmt.Errorf("lock workflow schema: %w", err)
		}
	}

	if _, err := transaction.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS workflow_schema_versions (version BIGINT PRIMARY KEY)"); err != nil {
		return fmt.Errorf("create workflow schema version table: %w", err)
	}

	entries, err := fs.ReadDir(schemaFiles, "schema")
	if err != nil {
		return fmt.Errorf("read workflow schema: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}

		version, err := schemaVersion(entry.Name())
		if err != nil {
			return err
		}

		var applied int
		if err := transaction.QueryRowContext(
			ctx, "SELECT COUNT(*) FROM workflow_schema_versions WHERE version = $1", version,
		).Scan(&applied); err != nil {
			return fmt.Errorf("read workflow schema version %d: %w", version, err)
		}

		if applied > 0 {
			continue
		}

		contents, err := fs.ReadFile(schemaFiles, "schema/"+entry.Name())
		if err != nil {
			return fmt.Errorf("read workflow schema version %d: %w", version, err)
		}

		if _, err := transaction.ExecContext(ctx, string(contents)); err != nil {
			return fmt.Errorf("apply workflow schema version %d: %w", version, err)
		}

		if _, err := transaction.ExecContext(
			ctx, "INSERT INTO workflow_schema_versions (version) VALUES ($1)", version,
		); err != nil {
			return fmt.Errorf("record workflow schema version %d: %w", version, err)
		}
	}

	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit workflow schema updates: %w", err)
	}

	return nil
}

func schemaVersion(name string) (int64, error) {
	prefix, _, found := strings.Cut(name, "_")
	if !found {
		return 0, fmt.Errorf("workflow schema file %s has no numeric prefix", name)
	}

	version, err := strconv.ParseInt(prefix, 10, 64)
	if err != nil || version <= 0 {
		return 0, fmt.Errorf("workflow schema file %s has an invalid version", name)
	}

	return version, nil
}
