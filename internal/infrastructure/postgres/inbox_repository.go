package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kassiobuck/backend-challenge-go/internal/application/port"
)

type InboxRepository struct {
	tx pgx.Tx
}

type InboxState = port.InboxState
type InboxReceipt = port.InboxReceipt

const (
	InboxReceived  = port.InboxReceived
	InboxCompleted = port.InboxCompleted
)

func (r *InboxRepository) Receive(
	ctx context.Context,
	consumerName, messageID, payloadHash string,
	receivedAt time.Time,
) (InboxReceipt, error) {
	tag, err := r.tx.Exec(ctx, `
		INSERT INTO inbox_messages (consumer_name, message_id, payload_hash, state, received_at)
		VALUES ($1, $2, $3, 'RECEIVED', $4)
		ON CONFLICT (consumer_name, message_id) DO NOTHING`,
		consumerName, messageID, payloadHash, receivedAt.UTC())
	if err != nil {
		return InboxReceipt{}, mapConstraintError(err)
	}
	receipt, err := r.find(ctx, consumerName, messageID)
	if err != nil {
		return InboxReceipt{}, err
	}
	if receipt.PayloadHash != payloadHash {
		return InboxReceipt{}, ErrInboxHashMismatch
	}
	receipt.Inserted = tag.RowsAffected() == 1
	return receipt, nil
}

func (r *InboxRepository) Complete(ctx context.Context, consumerName, messageID, payloadHash string, completedAt time.Time) error {
	tag, err := r.tx.Exec(ctx, `
		UPDATE inbox_messages
		SET state = 'COMPLETED', completed_at = $1
		WHERE consumer_name = $2 AND message_id = $3
			AND payload_hash = $4 AND state = 'RECEIVED'`,
		completedAt.UTC(), consumerName, messageID, payloadHash)
	if err != nil {
		return mapConstraintError(err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	receipt, err := r.find(ctx, consumerName, messageID)
	if err != nil {
		return err
	}
	if receipt.PayloadHash != payloadHash {
		return ErrInboxHashMismatch
	}
	if receipt.State == InboxCompleted {
		return nil
	}
	return ErrOptimisticLock
}

func (r *InboxRepository) find(ctx context.Context, consumerName, messageID string) (InboxReceipt, error) {
	var receipt InboxReceipt
	var completedAt *time.Time
	err := r.tx.QueryRow(ctx, `
		SELECT consumer_name, message_id, payload_hash, state, received_at, completed_at
		FROM inbox_messages WHERE consumer_name = $1 AND message_id = $2`,
		consumerName, messageID).Scan(&receipt.ConsumerName, &receipt.MessageID,
		&receipt.PayloadHash, &receipt.State, &receipt.ReceivedAt, &completedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return InboxReceipt{}, ErrNotFound
		}
		return InboxReceipt{}, fmt.Errorf("query inbox message: %w", err)
	}
	if completedAt != nil {
		utc := completedAt.UTC()
		receipt.CompletedAt = &utc
	}
	return receipt, nil
}
