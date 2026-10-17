package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/kassiobuck/backend-challenge-go/internal/domain/money"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/wager"
)

type WagerRepository struct {
	tx pgx.Tx
}

const wagerColumns = `
	t.id::text, t.origin, t.provider_id, t.external_transaction_id, t.idempotency_key,
	t.payload_hash, t.wallet_id::text, t.player_id::text, t.currency, t.round_id, t.game_id,
	t.kind, t.amount_minor, t.reference_external_transaction_id,
	t.resolved_reference_id::text, t.status, t.failure_code, t.result_balance_minor,
	t.created_at, t.updated_at`

func (r *WagerRepository) Create(ctx context.Context, entity *wager.WagerTransaction) error {
	if entity == nil {
		return errors.New("cannot persist nil wager transaction")
	}
	snapshot := entity.Snapshot()
	amount, err := snapshot.Amount.MinorUnits()
	if err != nil {
		return fmt.Errorf("read wager amount: %w", err)
	}
	currencyCode, err := currencyCode(snapshot.Amount)
	if err != nil {
		return fmt.Errorf("read wager currency: %w", err)
	}
	var resultBalance any
	if snapshot.HasResultBalance {
		resultBalance, err = snapshot.ResultBalance.MinorUnits()
		if err != nil {
			return fmt.Errorf("read wager result balance: %w", err)
		}
	}
	var resolvedReference any
	if snapshot.ResolvedReferenceID != "" {
		resolvedReference = snapshot.ResolvedReferenceID
	}
	var referenceNextAttempt any
	if snapshot.Status == wager.PendingReference {
		referenceNextAttempt = snapshot.CreatedAt
	}
	_, err = r.tx.Exec(ctx, `
		INSERT INTO wager_transactions (
			id, origin, provider_id, external_transaction_id, idempotency_key,
			payload_hash, wallet_id, player_id, currency, round_id, game_id, kind,
			amount_minor, reference_external_transaction_id, resolved_reference_id,
			status, failure_code, result_balance_minor, created_at, updated_at,
			reference_next_attempt_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
			$15, $16, $17, $18, $19, $20, $21
		)`,
		snapshot.ID, snapshot.Origin, nullable(snapshot.ProviderID),
		nullable(snapshot.ExternalTransactionID), nullable(snapshot.IdempotencyKey),
		nullable(snapshot.PayloadHash), snapshot.WalletID, snapshot.PlayerID,
		currencyCode, nullable(snapshot.RoundID), nullable(snapshot.GameID),
		snapshot.Kind, amount, nullable(snapshot.ReferenceExternalTransactionID),
		resolvedReference, snapshot.Status, nullable(string(snapshot.FailureCode)),
		resultBalance, snapshot.CreatedAt, snapshot.UpdatedAt, referenceNextAttempt)
	if err != nil {
		return mapConstraintError(err)
	}
	if snapshot.Origin == wager.External {
		_, err = r.tx.Exec(ctx, `
			INSERT INTO idempotency_records
				(provider_id, idempotency_key, payload_hash, transaction_id, created_at)
			VALUES ($1, $2, $3, $4, $5)`,
			snapshot.ProviderID, snapshot.IdempotencyKey, snapshot.PayloadHash,
			snapshot.ID, snapshot.CreatedAt)
		if err != nil {
			return mapConstraintError(err)
		}
	}
	return nil
}

func (r *WagerRepository) FindByID(ctx context.Context, id string) (*wager.WagerTransaction, error) {
	return r.find(ctx, `SELECT `+wagerColumns+` FROM wager_transactions t WHERE t.id = $1`, id)
}

func (r *WagerRepository) LockByID(ctx context.Context, id string) (*wager.WagerTransaction, error) {
	return r.find(ctx, `SELECT `+wagerColumns+` FROM wager_transactions t WHERE t.id = $1 FOR UPDATE OF t`, id)
}

func (r *WagerRepository) FindByExternalID(ctx context.Context, providerID, externalID string) (*wager.WagerTransaction, error) {
	return r.find(ctx, `SELECT `+wagerColumns+` FROM wager_transactions t
		WHERE t.origin = 'EXTERNAL' AND t.provider_id = $1 AND t.external_transaction_id = $2`,
		providerID, externalID)
}

func (r *WagerRepository) FindByIdempotencyKey(ctx context.Context, providerID, key string) (*wager.WagerTransaction, error) {
	return r.find(ctx, `SELECT `+wagerColumns+`
		FROM wager_transactions t
		JOIN idempotency_records i ON i.transaction_id = t.id
		WHERE i.provider_id = $1 AND i.idempotency_key = $2`,
		providerID, key)
}

func (r *WagerRepository) UpdateState(ctx context.Context, entity *wager.WagerTransaction) error {
	if entity == nil {
		return errors.New("cannot update nil wager transaction")
	}
	snapshot := entity.Snapshot()
	var resultBalance any
	var err error
	if snapshot.HasResultBalance {
		resultBalance, err = snapshot.ResultBalance.MinorUnits()
		if err != nil {
			return fmt.Errorf("read wager result balance: %w", err)
		}
	}
	var resolvedReference any
	if snapshot.ResolvedReferenceID != "" {
		resolvedReference = snapshot.ResolvedReferenceID
	}
	var referenceNextAttempt any
	if snapshot.Status == wager.PendingReference {
		referenceNextAttempt = snapshot.UpdatedAt
	}
	tag, err := r.tx.Exec(ctx, `
		UPDATE wager_transactions SET
			resolved_reference_id = $1, status = $2, failure_code = $3,
			result_balance_minor = $4, updated_at = $5,
			reference_next_attempt_at = $6
		WHERE id = $7`,
		resolvedReference, snapshot.Status, nullable(string(snapshot.FailureCode)),
		resultBalance, snapshot.UpdatedAt, referenceNextAttempt, snapshot.ID)
	if err != nil {
		return mapConstraintError(err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

func (r *WagerRepository) FindPendingReferences(ctx context.Context, limit int) ([]*wager.WagerTransaction, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("pending-reference limit must be positive")
	}
	rows, err := r.tx.Query(ctx, `SELECT `+wagerColumns+`
	FROM wager_transactions t
	WHERE t.status = 'PENDING_REFERENCE' AND t.reference_next_attempt_at <= clock_timestamp()
	ORDER BY t.reference_next_attempt_at, t.created_at, t.id
		LIMIT $1
	FOR UPDATE OF t SKIP LOCKED`, limit)
	if err != nil {
		return nil, fmt.Errorf("query pending wager references: %w", err)
	}
	defer rows.Close()
	return scanWagers(rows)
}

func (r *WagerRepository) SchedulePendingReferenceCheck(ctx context.Context, transactionID string, nextAttempt time.Time) error {
	tag, err := r.tx.Exec(ctx, `
		UPDATE wager_transactions
		SET reference_next_attempt_at = $1
		WHERE id = $2 AND status = 'PENDING_REFERENCE'`,
		nextAttempt.UTC(), transactionID)
	if err != nil {
		return mapConstraintError(err)
	}
	if tag.RowsAffected() != 1 {
		return ErrOptimisticLock
	}
	return nil
}

func (r *WagerRepository) find(ctx context.Context, query string, args ...any) (*wager.WagerTransaction, error) {
	var (
		id, origin, currencyCode, kind, status string
		walletID, playerID                     string
		providerID, externalID, idempotencyKey pgtype.Text
		payloadHash, roundID, gameID           pgtype.Text
		referenceExternal, resolvedReference   pgtype.Text
		failureCode                            pgtype.Text
		amount                                 int64
		resultBalance                          pgtype.Int8
		createdAt, updatedAt                   time.Time
	)
	err := r.tx.QueryRow(ctx, query, args...).Scan(
		&id, &origin, &providerID, &externalID, &idempotencyKey,
		&payloadHash, &walletID, &playerID, &currencyCode, &roundID, &gameID,
		&kind, &amount, &referenceExternal, &resolvedReference, &status,
		&failureCode, &resultBalance, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query wager transaction: %w", err)
	}
	var resultMinor int64
	if resultBalance.Valid {
		resultMinor = resultBalance.Int64
	}
	return hydrateWager(
		id, origin, providerID, externalID, idempotencyKey, payloadHash,
		walletID, playerID, currencyCode, roundID, gameID, kind, amount,
		referenceExternal, resolvedReference, status, failureCode, resultMinor,
		createdAt, updatedAt,
	)
}

func scanWagers(rows pgx.Rows) ([]*wager.WagerTransaction, error) {
	result := make([]*wager.WagerTransaction, 0)
	for rows.Next() {
		var (
			id, origin, currencyCode, kind, status string
			walletID, playerID                     string
			providerID, externalID, idempotencyKey pgtype.Text
			payloadHash, roundID, gameID           pgtype.Text
			referenceExternal, resolvedReference   pgtype.Text
			failureCode                            pgtype.Text
			amount                                 int64
			resultBalance                          pgtype.Int8
			createdAt, updatedAt                   time.Time
		)
		if err := rows.Scan(
			&id, &origin, &providerID, &externalID, &idempotencyKey,
			&payloadHash, &walletID, &playerID, &currencyCode, &roundID, &gameID,
			&kind, &amount, &referenceExternal, &resolvedReference, &status,
			&failureCode, &resultBalance, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan wager transaction: %w", err)
		}
		var minor int64
		if resultBalance.Valid {
			minor = resultBalance.Int64
		}
		entity, err := hydrateWager(
			id, origin, providerID, externalID, idempotencyKey, payloadHash,
			walletID, playerID, currencyCode, roundID, gameID, kind, amount,
			referenceExternal, resolvedReference, status, failureCode, minor,
			createdAt, updatedAt,
		)
		if err != nil {
			return nil, err
		}
		result = append(result, entity)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate wager transactions: %w", err)
	}
	return result, nil
}

func hydrateWager(
	id, origin string,
	providerID, externalID, idempotencyKey, payloadHash pgtype.Text,
	walletID, playerID, currencyCode string,
	roundID, gameID pgtype.Text,
	kind string,
	amountMinor int64,
	referenceExternal, resolvedReference pgtype.Text,
	status string,
	failureCode pgtype.Text,
	resultBalanceMinor int64,
	createdAt, updatedAt time.Time,
) (*wager.WagerTransaction, error) {
	currency, err := money.NewCurrency(currencyCode)
	if err != nil {
		return nil, fmt.Errorf("%w: wager currency", ErrCorruptRow)
	}
	amount, err := money.NewMinor(amountMinor, currency)
	if err != nil {
		return nil, fmt.Errorf("%w: wager amount", ErrCorruptRow)
	}
	snapshot := wager.Snapshot{
		Origin: wager.Origin(origin), ID: id, ProviderID: textValue(providerID),
		ExternalTransactionID: textValue(externalID), IdempotencyKey: textValue(idempotencyKey),
		PayloadHash: textValue(payloadHash), WalletID: walletID, PlayerID: playerID,
		RoundID: textValue(roundID), GameID: textValue(gameID), Kind: wager.Kind(kind),
		Amount: amount, ReferenceExternalTransactionID: textValue(referenceExternal),
		ResolvedReferenceID: textValue(resolvedReference), Status: wager.Status(status),
		FailureCode: wager.FailureCode(textValue(failureCode)),
		CreatedAt:   createdAt, UpdatedAt: updatedAt,
	}
	if status == string(wager.Processed) {
		snapshot.ResultBalance, err = money.NewMinor(resultBalanceMinor, currency)
		if err != nil {
			return nil, fmt.Errorf("%w: wager result balance", ErrCorruptRow)
		}
		snapshot.HasResultBalance = true
	}
	entity, err := wager.Rehydrate(snapshot)
	if err != nil {
		return nil, fmt.Errorf("rehydrate wager transaction: %w: %v", ErrCorruptRow, err)
	}
	return entity, nil
}

func currencyCode(amount money.Money) (string, error) {
	currency, err := amount.Currency()
	if err != nil {
		return "", err
	}
	return string(currency), nil
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func textValue(value pgtype.Text) string {
	if !value.Valid {
		return ""
	}
	return value.String
}
