package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/kassiobuck/backend-challenge-go/internal/application/port"
)

type IdempotencyRepository struct {
	tx pgx.Tx
}

type IdempotencyRecord = port.IdempotencyRecord

func (r *IdempotencyRepository) Find(ctx context.Context, providerID, key string) (IdempotencyRecord, error) {
	var record IdempotencyRecord
	err := r.tx.QueryRow(ctx, `
		SELECT provider_id, idempotency_key, payload_hash, transaction_id::text,
			created_at
		FROM idempotency_records WHERE provider_id = $1 AND idempotency_key = $2`,
		providerID, key).Scan(&record.ProviderID, &record.Key, &record.PayloadHash,
		&record.TransactionID, &record.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return IdempotencyRecord{}, ErrNotFound
		}
		return IdempotencyRecord{}, fmt.Errorf("query idempotency record: %w", err)
	}
	return record, nil
}
