package postgres

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kassiobuck/backend-challenge-go/internal/application/port"
)

type OutboxRepository struct {
	tx pgx.Tx
}

type OutboxEvent = port.OutboxEvent

func (r *OutboxRepository) Append(ctx context.Context, event OutboxEvent) error {
	createdAt := event.CreatedAt
	if createdAt.IsZero() {
		createdAt = event.OccurredAt
	}
	availableAt := event.AvailableAt
	if availableAt.IsZero() {
		availableAt = createdAt
	}
	_, err := r.tx.Exec(ctx, `
		INSERT INTO outbox_events (
			event_id, aggregate_id, event_type, schema_version, correlation_id,
			causation_id, payload, occurred_at, attempts, available_at, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 0, $9, $10)`,
		event.EventID, event.AggregateID, event.EventType, event.SchemaVersion,
		event.CorrelationID, nullable(event.CausationID), []byte(event.Payload),
		event.OccurredAt.UTC(), availableAt.UTC(), createdAt.UTC())
	return mapConstraintError(err)
}

func (r *OutboxRepository) ClaimBatch(
	ctx context.Context,
	owner string,
	now time.Time,
	lease time.Duration,
	limit int,
) ([]OutboxEvent, error) {
	if strings.TrimSpace(owner) == "" || lease.Microseconds() <= 0 || limit <= 0 {
		return nil, fmt.Errorf("outbox claim requires owner, lease of at least one microsecond, and positive limit")
	}
	leaseOwner, err := newLeaseOwner(owner)
	if err != nil {
		return nil, err
	}
	rows, err := r.tx.Query(ctx, `
		WITH candidates AS (
			SELECT event_id FROM outbox_events
			WHERE published_at IS NULL AND available_at <= $1
				AND (locked_until IS NULL OR locked_until <= $1)
			ORDER BY available_at, occurred_at, event_id
			FOR UPDATE SKIP LOCKED
			LIMIT $2
		)
		UPDATE outbox_events AS event
		SET lock_owner = $3, locked_until = $1 + ($4 * interval '1 microsecond'),
			attempts = event.attempts + 1
		FROM candidates
		WHERE event.event_id = candidates.event_id
		RETURNING event.event_id::text, event.aggregate_id::text, event.event_type,
			event.schema_version, event.correlation_id::text, event.causation_id::text,
			event.payload, event.occurred_at, event.attempts, event.available_at,
			event.lock_owner, event.locked_until, event.published_at,
			COALESCE(event.last_error, ''), event.created_at`,
		now.UTC(), limit, leaseOwner, lease.Microseconds())
	if err != nil {
		return nil, fmt.Errorf("claim outbox events: %w", err)
	}

	defer rows.Close()
	events := make([]OutboxEvent, 0)
	for rows.Next() {
		var event OutboxEvent
		var causationID *string
		if err := rows.Scan(
			&event.EventID, &event.AggregateID, &event.EventType, &event.SchemaVersion,
			&event.CorrelationID, &causationID, &event.Payload, &event.OccurredAt,
			&event.Attempts, &event.AvailableAt, &event.LockOwner, &event.LockedUntil,
			&event.PublishedAt, &event.LastError, &event.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan claimed outbox event: %w", err)
		}
		if causationID != nil {
			event.CausationID = *causationID
		}
		event.Payload = append(json.RawMessage(nil), event.Payload...)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claimed outbox events: %w", err)
	}
	return events, nil
}

func newLeaseOwner(owner string) (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate outbox lease token: %w", err)
	}
	random[6] = (random[6] & 0x0f) | 0x40
	random[8] = (random[8] & 0x3f) | 0x80
	token := fmt.Sprintf("%x-%x-%x-%x-%x",
		random[0:4], random[4:6], random[6:8], random[8:10], random[10:16])
	return owner + ":" + token, nil
}

func (r *OutboxRepository) MarkPublished(ctx context.Context, eventID, owner string, publishedAt time.Time) error {
	tag, err := r.tx.Exec(ctx, `
		UPDATE outbox_events
		SET published_at = $1, lock_owner = NULL, locked_until = NULL, last_error = NULL
		WHERE event_id = $2 AND lock_owner = $3 AND published_at IS NULL`,
		publishedAt.UTC(), eventID, owner)
	if err != nil {
		return mapConstraintError(err)
	}
	if tag.RowsAffected() != 1 {
		return ErrOutboxLeaseLost
	}
	return nil
}

func (r *OutboxRepository) Reschedule(ctx context.Context, eventID, owner, failure string, retryAt time.Time) error {
	tag, err := r.tx.Exec(ctx, `
		UPDATE outbox_events
		SET available_at = $1, lock_owner = NULL, locked_until = NULL, last_error = $2
		WHERE event_id = $3 AND lock_owner = $4 AND published_at IS NULL`,
		retryAt.UTC(), failure, eventID, owner)
	if err != nil {
		return mapConstraintError(err)
	}
	if tag.RowsAffected() != 1 {
		return ErrOutboxLeaseLost
	}
	return nil
}
