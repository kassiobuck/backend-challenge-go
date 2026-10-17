package postgres

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kassiobuck/backend-challenge-go/internal/application/port"
)

var (
	ErrNotFound             = port.ErrNotFound
	ErrConflict             = port.ErrConflict
	ErrConstraintViolation  = port.ErrConstraintViolation
	ErrOptimisticLock       = port.ErrOptimisticLock
	ErrCorruptRow           = errors.New("invalid persisted domain row")
	ErrInboxHashMismatch    = port.ErrInboxHashMismatch
	ErrOutboxLeaseLost      = errors.New("outbox lease is no longer owned")
	ErrRetryableTransaction = port.ErrRetryableTransaction
)

func mapConstraintError(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505", "23P01":
			return fmt.Errorf("%w (%s): %w", ErrConflict, pgErr.ConstraintName, err)
		case "23503", "23514", "23502", "22003":
			return fmt.Errorf("%w (%s): %w", ErrConstraintViolation, pgErr.ConstraintName, err)
		case "40001", "40P01":
			return fmt.Errorf("%w: %w", ErrRetryableTransaction, err)
		}
	}
	return err
}
