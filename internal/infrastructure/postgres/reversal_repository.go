package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type ReversalRepository struct {
	tx pgx.Tx
}

func (r *ReversalRepository) RecordProcessed(
	ctx context.Context,
	referenceTransactionID, reversalTransactionID string,
	at time.Time,
) error {
	_, err := r.tx.Exec(ctx, `
		INSERT INTO wager_reversals (reference_transaction_id, reversal_transaction_id, created_at)
		VALUES ($1, $2, $3)`,
		referenceTransactionID, reversalTransactionID, at.UTC())
	if err != nil {
		return mapConstraintError(err)
	}
	return nil
}

func (r *ReversalRepository) FindByReference(ctx context.Context, referenceTransactionID string) (string, error) {
	var reversalID string
	err := r.tx.QueryRow(ctx, `
		SELECT reversal_transaction_id::text FROM wager_reversals
		WHERE reference_transaction_id = $1`, referenceTransactionID).Scan(&reversalID)
	if err != nil {
		if err == pgx.ErrNoRows {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("query wager reversal: %w", err)
	}
	return reversalID, nil
}
