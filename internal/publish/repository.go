package publish

import (
	"context"
	"errors"
)

var (
	// ErrNotFound means the requested publication record does not exist.
	ErrNotFound = errors.New("publication not found")
	// ErrConflict means the session already owns a different publication intent.
	ErrConflict = errors.New("publication conflict")
)

// Repository owns durable publish intent, progress, and results.
type Repository interface {
	Create(context.Context, Record) (Record, error)
	Get(context.Context, string) (Record, error)
	Update(context.Context, Record) (Record, error)
}
