package storage

import (
	"context"
	stdsql "database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/yarlson/dyne/internal/publish"
	"github.com/yarlson/dyne/internal/storage/sql"
)

// PublicationRepository owns durable publish intent, progress, and results.
type PublicationRepository struct {
	queries *sql.Queries
}

// Publications returns the publish aggregate repository.
func (d *Database) Publications() *PublicationRepository {
	return &PublicationRepository{queries: d.queries}
}

// Create inserts one publish intent.
func (r *PublicationRepository) Create(ctx context.Context, record publish.Record) (publish.Record, error) {
	contents, err := json.Marshal(record)
	if err != nil {
		return publish.Record{}, fmt.Errorf("encode publication for session %s: %w", record.Session, err)
	}

	row, err := r.queries.CreatePublication(ctx, sql.CreatePublicationParams{
		SessionName: record.Session, Contents: contents,
	})
	if err != nil {
		if _, getErr := r.queries.GetPublication(ctx, record.Session); getErr == nil {
			return publish.Record{}, publish.ErrConflict
		}

		return publish.Record{}, fmt.Errorf("create publication for session %s: %w", record.Session, err)
	}

	return decodePublication(row)
}

// Get returns one durable publish record.
func (r *PublicationRepository) Get(ctx context.Context, sessionName string) (publish.Record, error) {
	row, err := r.queries.GetPublication(ctx, sessionName)
	if errors.Is(err, stdsql.ErrNoRows) {
		return publish.Record{}, publish.ErrNotFound
	}

	if err != nil {
		return publish.Record{}, fmt.Errorf("get publication for session %s: %w", sessionName, err)
	}

	return decodePublication(row)
}

// Update replaces one publish record only when its revision is current.
func (r *PublicationRepository) Update(ctx context.Context, record publish.Record) (publish.Record, error) {
	contents, err := json.Marshal(record)
	if err != nil {
		return publish.Record{}, fmt.Errorf("encode publication for session %s: %w", record.Session, err)
	}

	row, err := r.queries.UpdatePublication(ctx, sql.UpdatePublicationParams{
		SessionName: record.Session, Contents: contents, Version: record.Revision,
	})
	if errors.Is(err, stdsql.ErrNoRows) {
		return publish.Record{}, publish.ErrConflict
	}

	if err != nil {
		return publish.Record{}, fmt.Errorf("update publication for session %s: %w", record.Session, err)
	}

	return decodePublication(row)
}

func decodePublication(row sql.Publication) (publish.Record, error) {
	var record publish.Record
	if err := json.Unmarshal(row.Contents, &record); err != nil {
		return publish.Record{}, fmt.Errorf("decode publication for session %s: %w", row.SessionName, err)
	}

	if record.Session != row.SessionName {
		return publish.Record{}, fmt.Errorf("publication for session %s has an invalid durable identity", row.SessionName)
	}

	record.Revision = row.Version

	return record, nil
}
