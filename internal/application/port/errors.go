package port

import "errors"

var (
	ErrNotFound             = errors.New("repository record not found")
	ErrConflict             = errors.New("repository uniqueness conflict")
	ErrConstraintViolation  = errors.New("repository constraint violation")
	ErrOptimisticLock       = errors.New("repository optimistic lock conflict")
	ErrRetryableTransaction = errors.New("repository transaction should be retried")
	ErrInboxHashMismatch    = errors.New("inbox message ID was reused with different content")
)
