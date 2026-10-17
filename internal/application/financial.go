package application

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kassiobuck/backend-challenge-go/internal/application/port"
	"github.com/kassiobuck/backend-challenge-go/internal/domain"
	domainevent "github.com/kassiobuck/backend-challenge-go/internal/domain/event"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/ledger"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/money"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/wager"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/wallet"
	"github.com/kassiobuck/backend-challenge-go/internal/messaging"
)

var (
	ErrIdempotencyConflict         = errors.New("idempotency key was reused with different content")
	ErrExternalTransactionConflict = errors.New("external transaction was reused with different content")
	ErrWalletAlreadyExists         = errors.New("wallet already exists for player and currency")
	ErrWalletOwnership             = errors.New("wallet does not belong to player")
	ErrRetryableConflict           = errors.New("financial operation conflicted; retry the request")
)

type IDGenerator func() (string, error)
type Clock func() time.Time

type FinancialService struct {
	unitOfWork port.UnitOfWork
	newID      IDGenerator
	now        Clock
}

func NewFinancialService(unitOfWork port.UnitOfWork) *FinancialService {
	return NewFinancialServiceWith(unitOfWork, newUUID, time.Now)
}

func NewFinancialServiceWith(unitOfWork port.UnitOfWork, newID IDGenerator, now Clock) *FinancialService {
	return &FinancialService{unitOfWork: unitOfWork, newID: newID, now: now}
}

type OpenWalletCommand struct {
	PlayerID       string
	InitialBalance money.Money
}

type WalletOpened struct {
	Wallet       *wallet.Wallet
	OpeningWager *wager.WagerTransaction
	Replay       bool
}

type ProcessWagerCommand struct {
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	WalletID                       string
	PlayerID                       string
	RoundID                        string
	GameID                         string
	Kind                           wager.Kind
	Amount                         money.Money
	ReferenceExternalTransactionID string
	CorrelationID                  string
}

type WagerResult struct {
	Transaction *wager.WagerTransaction
	LedgerEntry *ledger.WalletLedgerEntry
	Replay      bool
}

type processWagerParams struct {
	service        *FinancialService
	command        ProcessWagerCommand
	hash           string
	inboxConsumer  string
	inboxMessageID string
}

func (s *FinancialService) OpenWallet(ctx context.Context, command OpenWalletCommand) (*WalletOpened, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	command.PlayerID = strings.ToLower(strings.TrimSpace(command.PlayerID))
	if !command.InitialBalance.IsValid() || command.InitialBalance.IsNegative() {
		return nil, domain.ErrInvalidWallet
	}
	now := s.timestamp()
	walletID, err := s.newID()
	if err != nil {
		return nil, fmt.Errorf("generate wallet id: %w", err)
	}
	entity, err := wallet.Open(walletID, command.PlayerID, command.InitialBalance, now)
	if err != nil {
		return nil, err
	}
	var opening *wager.WagerTransaction
	var openingEntry *ledger.WalletLedgerEntry
	if command.InitialBalance.IsPositive() {
		transactionID, err := s.newID()
		if err != nil {
			return nil, fmt.Errorf("generate opening transaction id: %w", err)
		}
		opening, err = wager.NewOpening(wager.OpeningParams{
			ID: transactionID, WalletID: walletID, PlayerID: command.PlayerID,
			Amount: command.InitialBalance, CreatedAt: now,
		})
		if err != nil {
			return nil, err
		}
		entryID, err := s.newID()
		if err != nil {
			return nil, fmt.Errorf("generate opening ledger id: %w", err)
		}
		currency, err := command.InitialBalance.Currency()
		if err != nil {
			return nil, err
		}
		zero, err := money.Zero(currency)
		if err != nil {
			return nil, err
		}
		entry, err := ledger.NewEntry(entryID, walletID, transactionID, ledger.Credit,
			command.InitialBalance, zero, command.InitialBalance, now)
		if err != nil {
			return nil, err
		}
		openingEntry = &entry
	}
	err = s.unitOfWork.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
		if err := repos.Wallets().Create(ctx, entity); err != nil {
			return err
		}
		if opening == nil {
			return nil
		}
		if err := repos.Wagers().Create(ctx, opening); err != nil {
			return err
		}
		if err := repos.Ledger().Append(ctx, *openingEntry, 1); err != nil {
			return err
		}
		return s.appendProcessedEvents(ctx, repos, opening, openingEntry, 1, "")
	})
	if errors.Is(err, port.ErrConflict) {
		return nil, ErrWalletAlreadyExists
	}
	if err != nil {
		return nil, fmt.Errorf("open wallet transaction: %w", err)
	}
	return &WalletOpened{Wallet: entity, OpeningWager: opening}, nil
}

func (s *FinancialService) ProcessWager(ctx context.Context, input ProcessWagerCommand) (*WagerResult, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	command := normalizeCommand(input)
	hash, err := payloadHash(command)
	if err != nil {
		return nil, err
	}
	params := processWagerParams{service: s, command: command, hash: hash}
	return params.process(ctx)
}

func (s *FinancialService) ProcessWagerMessage(
	ctx context.Context,
	consumerName, messageID string,
	input ProcessWagerCommand,
) (*WagerResult, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(consumerName) == "" || strings.TrimSpace(messageID) == "" {
		return nil, errors.New("inbox consumer name and message ID are required")
	}
	command := normalizeCommand(input)
	hash, err := payloadHash(command)
	if err != nil {
		return nil, err
	}
	return (processWagerParams{
		service: s, command: command, hash: hash,
		inboxConsumer: strings.TrimSpace(consumerName), inboxMessageID: strings.TrimSpace(messageID),
	}).process(ctx)
}

func (p processWagerParams) process(ctx context.Context) (*WagerResult, error) {
	result, err := p.execute(ctx)
	if errors.Is(err, port.ErrConflict) || errors.Is(err, port.ErrOptimisticLock) ||
		errors.Is(err, port.ErrRetryableTransaction) {
		replayed, replayErr := p.findReplay(ctx)
		if replayErr == nil {
			return replayed, nil
		}
		if !errors.Is(replayErr, port.ErrNotFound) {
			return nil, replayErr
		}
		if errors.Is(err, port.ErrOptimisticLock) || errors.Is(err, port.ErrRetryableTransaction) {
			return nil, fmt.Errorf("%w: %v", ErrRetryableConflict, err)
		}
	}
	return result, err
}

func (p processWagerParams) execute(ctx context.Context) (*WagerResult, error) {
	s := p.service
	var result *WagerResult
	err := s.unitOfWork.Within(ctx, func(ctx context.Context, repos port.Repositories) (callbackErr error) {
		if p.inboxConsumer != "" {
			receipt, err := repos.Inbox().Receive(ctx, p.inboxConsumer, p.inboxMessageID, p.hash, s.timestamp())
			if err != nil {
				return err
			}
			if receipt.State == port.InboxCompleted {
				existing, err := repos.Wagers().FindByIdempotencyKey(
					ctx, p.command.ProviderID, p.command.IdempotencyKey)
				if err != nil {
					return err
				}
				return replayOrConflict(repos, ctx, existing, p.hash, p.command.IdempotencyKey, false, &result)
			}
			defer func() {
				if callbackErr == nil {
					callbackErr = repos.Inbox().Complete(
						ctx, p.inboxConsumer, p.inboxMessageID, p.hash, s.timestamp())
				}
			}()
		}
		if existing, err := repos.Wagers().FindByIdempotencyKey(
			ctx, p.command.ProviderID, p.command.IdempotencyKey); err == nil {
			return replayOrConflict(repos, ctx, existing, p.hash, p.command.IdempotencyKey, false, &result)
		} else if !errors.Is(err, port.ErrNotFound) {
			return err
		}
		if existing, err := repos.Wagers().FindByExternalID(
			ctx, p.command.ProviderID, p.command.ExternalTransactionID); err == nil {
			return replayOrConflict(repos, ctx, existing, p.hash, p.command.IdempotencyKey, true, &result)
		} else if !errors.Is(err, port.ErrNotFound) {
			return err
		}

		transactionID, err := s.newID()
		if err != nil {
			return fmt.Errorf("generate wager transaction id: %w", err)
		}
		transaction, err := wager.NewExternal(wager.ExternalParams{
			ID: transactionID, ProviderID: p.command.ProviderID,
			ExternalTransactionID: p.command.ExternalTransactionID,
			IdempotencyKey:        p.command.IdempotencyKey, PayloadHash: p.hash,
			WalletID: p.command.WalletID, PlayerID: p.command.PlayerID,
			RoundID: p.command.RoundID, GameID: p.command.GameID,
			Kind: p.command.Kind, Amount: p.command.Amount,
			ReferenceExternalTransactionID: p.command.ReferenceExternalTransactionID,
			CreatedAt:                      s.timestamp(),
		})
		if err != nil {
			return err
		}

		lockedWallet, err := repos.Wallets().LockByID(ctx, p.command.WalletID)
		if err != nil {
			return err
		}
		if lockedWallet.PlayerID() != p.command.PlayerID {
			return ErrWalletOwnership
		}
		var reference *wager.WagerTransaction
		if p.command.ReferenceExternalTransactionID != "" {
			reference, err = repos.Wagers().FindByExternalID(
				ctx, p.command.ProviderID, p.command.ReferenceExternalTransactionID)
			if err != nil && !errors.Is(err, port.ErrNotFound) {
				return err
			}
			if errors.Is(err, port.ErrNotFound) || isReferencePending(reference) {
				if transitionErr := transaction.WaitForReference(s.timestamp()); transitionErr != nil {
					return transitionErr
				}
				if createErr := repos.Wagers().Create(ctx, transaction); createErr != nil {
					return createErr
				}
				if eventErr := s.appendPendingEvent(ctx, repos, transaction, p.command.CorrelationID); eventErr != nil {
					return eventErr
				}
				result = &WagerResult{Transaction: transaction}
				return nil
			}
		}

		return s.applyWager(ctx, repos, transaction, lockedWallet, reference, p.command.CorrelationID, &result)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *FinancialService) applyWager(
	ctx context.Context,
	repos port.Repositories,
	transaction *wager.WagerTransaction,
	lockedWallet *wallet.Wallet,
	reference *wager.WagerTransaction,
	correlationID string,
	result **WagerResult,
) error {
	alreadyPersisted := transaction.Status() == wager.PendingReference
	now := s.timestamp()
	direction, moves, err := transaction.Movement(reference)
	if err != nil {
		if errors.Is(err, domain.ErrReferencePending) {
			if alreadyPersisted {
				*result = &WagerResult{Transaction: transaction}
				return nil
			}
			if err := transaction.WaitForReference(now); err != nil {
				return err
			}
			if err := persistWager(ctx, repos, transaction, alreadyPersisted); err != nil {
				return err
			}
			if err := s.appendPendingEvent(ctx, repos, transaction, correlationID); err != nil {
				return err
			}
			*result = &WagerResult{Transaction: transaction}
			return nil
		}
		if rejectErr := transaction.Reject(referenceFailureCode(err), now); rejectErr != nil {
			return rejectErr
		}
		if err := persistWager(ctx, repos, transaction, alreadyPersisted); err != nil {
			return err
		}
		if err := s.appendRejectedEvent(ctx, repos, transaction, correlationID); err != nil {
			return err
		}
		*result = &WagerResult{Transaction: transaction}
		return nil
	}

	var change wallet.BalanceChange
	if direction == ledger.Debit {
		if transaction.Kind() == wager.Rollback {
			change, err = lockedWallet.DebitForReversal(transaction.Amount(), now)
		} else {
			change, err = lockedWallet.Debit(transaction.Amount(), now)
		}
	} else if moves {
		change, err = lockedWallet.Credit(transaction.Amount(), now)
	}
	if err != nil {
		if !isBusinessRejection(err) {
			return err
		}
		if rejectErr := transaction.Reject(failureCode(err), now); rejectErr != nil {
			return rejectErr
		}
		if createErr := persistWager(ctx, repos, transaction, alreadyPersisted); createErr != nil {
			return createErr
		}
		if eventErr := s.appendRejectedEvent(ctx, repos, transaction, correlationID); eventErr != nil {
			return eventErr
		}
		*result = &WagerResult{Transaction: transaction}
		return nil
	}

	before := lockedWallet.Balance()
	after := lockedWallet.Balance()
	version := lockedWallet.Version()
	var entry *ledger.WalletLedgerEntry
	if moves {
		before, after, version = change.Before(), change.After(), change.Version()
		entryID, idErr := s.newID()
		if idErr != nil {
			return fmt.Errorf("generate ledger entry id: %w", idErr)
		}
		created, createErr := ledger.NewEntry(entryID, lockedWallet.ID(), transaction.ID(),
			direction, transaction.Amount(), before, after, now)
		if createErr != nil {
			return createErr
		}
		entry = &created
	}
	if err := transaction.Process(before, after, reference, now); err != nil {
		return err
	}
	if err := persistWager(ctx, repos, transaction, alreadyPersisted); err != nil {
		return err
	}
	if moves {
		if err := repos.Wallets().SaveBalance(ctx, lockedWallet.ID(), version-1, after, version, now); err != nil {
			return err
		}
		if err := repos.Ledger().Append(ctx, *entry, version); err != nil {
			return err
		}
	}
	if transaction.Kind() == wager.Refund || transaction.Kind() == wager.Rollback {
		if err := repos.Reversals().RecordProcessed(ctx, transaction.ResolvedReferenceID(), transaction.ID(), now); err != nil {
			return err
		}
	}
	if err := s.appendProcessedEvents(ctx, repos, transaction, entry, version, correlationID); err != nil {
		return err
	}
	*result = &WagerResult{Transaction: transaction, LedgerEntry: entry}
	return nil
}

func persistWager(ctx context.Context, repos port.Repositories, transaction *wager.WagerTransaction, alreadyPersisted bool) error {
	if alreadyPersisted {
		return repos.Wagers().UpdateState(ctx, transaction)
	}
	return repos.Wagers().Create(ctx, transaction)
}

func (p processWagerParams) findReplay(ctx context.Context) (*WagerResult, error) {
	var result *WagerResult
	err := p.service.unitOfWork.Within(ctx, func(ctx context.Context, repos port.Repositories) (callbackErr error) {
		if p.inboxConsumer != "" {
			receipt, err := repos.Inbox().Receive(
				ctx, p.inboxConsumer, p.inboxMessageID, p.hash, p.service.timestamp())
			if err != nil {
				return err
			}
			if receipt.State == port.InboxCompleted {
				transaction, err := repos.Wagers().FindByIdempotencyKey(
					ctx, p.command.ProviderID, p.command.IdempotencyKey)
				if err != nil {
					return err
				}
				return replayOrConflict(
					repos, ctx, transaction, p.hash, p.command.IdempotencyKey, false, &result)
			}
			defer func() {
				if callbackErr == nil {
					callbackErr = repos.Inbox().Complete(
						ctx, p.inboxConsumer, p.inboxMessageID, p.hash, p.service.timestamp())
				}
			}()
		}
		transaction, err := repos.Wagers().FindByIdempotencyKey(ctx, p.command.ProviderID, p.command.IdempotencyKey)
		if err == nil {
			return replayOrConflict(repos, ctx, transaction, p.hash, p.command.IdempotencyKey, false, &result)
		}
		if !errors.Is(err, port.ErrNotFound) {
			return err
		}
		transaction, err = repos.Wagers().FindByExternalID(ctx, p.command.ProviderID, p.command.ExternalTransactionID)
		if err != nil {
			return err
		}
		return replayOrConflict(repos, ctx, transaction, p.hash, p.command.IdempotencyKey, true, &result)
	})
	return result, err
}

func replayOrConflict(
	repos port.Repositories,
	ctx context.Context,
	existing *wager.WagerTransaction,
	hash string,
	idempotencyKey string,
	byExternalID bool,
	result **WagerResult,
) error {
	if existing.Snapshot().PayloadHash != hash || (byExternalID && existing.Snapshot().IdempotencyKey != idempotencyKey) {
		if byExternalID {
			return ErrExternalTransactionConflict
		}
		return ErrIdempotencyConflict
	}
	var entry *ledger.WalletLedgerEntry
	if existing.Status() == wager.Processed && existing.Kind() != wager.Loss {
		persisted, err := repos.Ledger().FindByTransactionID(ctx, existing.WalletID(), existing.ID())
		if err != nil {
			return err
		}
		entry = &persisted
	}
	*result = &WagerResult{Transaction: existing, LedgerEntry: entry, Replay: true}
	return nil
}

func (s *FinancialService) ResolvePendingReferences(ctx context.Context, limit int) (int, error) {
	if err := s.validate(); err != nil {
		return 0, err
	}
	if limit <= 0 {
		return 0, errors.New("pending reference limit must be positive")
	}
	resolved := 0
	err := s.unitOfWork.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
		pending, err := repos.Wagers().FindPendingReferences(ctx, limit)
		if err != nil {
			return err
		}
		for _, transaction := range pending {
			reference, err := repos.Wagers().FindByExternalID(ctx,
				transaction.ProviderID(), transaction.ReferenceExternalTransactionID())
			if errors.Is(err, port.ErrNotFound) || (err == nil && isReferencePending(reference)) {
				if scheduleErr := repos.Wagers().SchedulePendingReferenceCheck(
					ctx, transaction.ID(), s.timestamp().Add(5*time.Second)); scheduleErr != nil {
					return scheduleErr
				}
				continue
			}
			if err != nil {
				return err
			}
			walletEntity, err := repos.Wallets().LockByID(ctx, transaction.WalletID())
			if err != nil {
				return err
			}
			if err := s.applyWager(ctx, repos, transaction, walletEntity, reference,
				transaction.ID(), new(*WagerResult)); err != nil {
				return err
			}
			if transaction.Status() == wager.Processed || transaction.Status() == wager.Rejected {
				resolved++
			}
		}
		return nil
	})
	return resolved, err
}

func (s *FinancialService) appendProcessedEvents(
	ctx context.Context,
	repos port.Repositories,
	transaction *wager.WagerTransaction,
	entry *ledger.WalletLedgerEntry,
	version uint64,
	correlationID string,
) error {
	if err := s.appendEvent(ctx, repos, eventSpec{
		eventType: stringEventProcessed, transaction: transaction, correlationID: correlationID,
	}); err != nil {
		return err
	}
	if entry == nil {
		return nil
	}
	return s.appendEvent(ctx, repos, eventSpec{
		eventType: stringEventBalanceChanged, transaction: transaction, entry: entry,
		version: version, correlationID: correlationID,
	})
}

func (s *FinancialService) appendRejectedEvent(ctx context.Context, repos port.Repositories, transaction *wager.WagerTransaction, correlationID string) error {
	return s.appendEvent(ctx, repos, eventSpec{
		eventType: stringEventRejected, transaction: transaction, correlationID: correlationID,
	})
}

func (s *FinancialService) appendPendingEvent(ctx context.Context, repos port.Repositories, transaction *wager.WagerTransaction, correlationID string) error {
	return s.appendEvent(ctx, repos, eventSpec{
		eventType: stringEventPendingReference, transaction: transaction, correlationID: correlationID,
	})
}

const (
	stringEventProcessed        = string(domainevent.WagerTransactionProcessedType)
	stringEventRejected         = string(domainevent.WagerTransactionRejectedType)
	stringEventBalanceChanged   = string(domainevent.WalletBalanceChangedType)
	stringEventPendingReference = string(domainevent.WagerTransactionPendingReferenceType)
)

type eventSpec struct {
	eventType     string
	transaction   *wager.WagerTransaction
	entry         *ledger.WalletLedgerEntry
	version       uint64
	correlationID string
}

func (s *FinancialService) appendEvent(ctx context.Context, repos port.Repositories, spec eventSpec) error {
	eventID, err := s.newID()
	if err != nil {
		return fmt.Errorf("generate outbox event id: %w", err)
	}
	correlationID := strings.TrimSpace(spec.correlationID)
	if correlationID == "" {
		correlationID = spec.transaction.ID()
	}
	snapshot := spec.transaction.Snapshot()
	occurredAt := s.timestamp()
	metadata := messaging.NewEventMetadata(eventID, spec.eventType,
		spec.transaction.WalletID(), correlationID, spec.transaction.ID(), occurredAt)
	var payload any
	switch spec.eventType {
	case stringEventProcessed:
		balance, _ := spec.transaction.ResultBalance()
		payload = messaging.EventEnvelope[messaging.WagerTransactionProcessedData]{
			EventMetadata: metadata,
			Data: messaging.WagerTransactionProcessedData{
				TransactionID: snapshot.ID, ProviderID: snapshot.ProviderID,
				ExternalTransactionID: snapshot.ExternalTransactionID, Kind: snapshot.Kind,
				Amount: snapshot.Amount, ResultingBalance: balance,
			},
		}
	case stringEventRejected:
		payload = messaging.EventEnvelope[messaging.WagerTransactionRejectedData]{
			EventMetadata: metadata,
			Data: messaging.WagerTransactionRejectedData{
				TransactionID: snapshot.ID, ProviderID: snapshot.ProviderID,
				ExternalTransactionID: snapshot.ExternalTransactionID,
				Kind:                  snapshot.Kind, FailureCode: snapshot.FailureCode,
			},
		}
	case stringEventPendingReference:
		payload = messaging.EventEnvelope[messaging.WagerTransactionPendingReferenceData]{
			EventMetadata: metadata,
			Data: messaging.WagerTransactionPendingReferenceData{
				TransactionID: snapshot.ID, ProviderID: snapshot.ProviderID,
				ExternalTransactionID:          snapshot.ExternalTransactionID,
				ReferenceExternalTransactionID: snapshot.ReferenceExternalTransactionID,
				Kind:                           snapshot.Kind, Amount: snapshot.Amount,
			},
		}
	case stringEventBalanceChanged:
		payload = messaging.EventEnvelope[messaging.WalletBalanceChangedData]{
			EventMetadata: metadata,
			Data: messaging.WalletBalanceChangedData{
				WalletID: spec.entry.WalletID(), TransactionID: spec.entry.TransactionID(),
				Direction: spec.entry.Direction(), Amount: spec.entry.Amount(),
				BalanceBefore: spec.entry.BalanceBefore(), BalanceAfter: spec.entry.BalanceAfter(),
				WalletVersion: spec.version,
			},
		}
	default:
		return fmt.Errorf("unsupported outbox event type %q", spec.eventType)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode outbox event: %w", err)
	}
	return repos.Outbox().Append(ctx, port.OutboxEvent{
		EventID: eventID, AggregateID: spec.transaction.WalletID(),
		EventType: spec.eventType, SchemaVersion: 1, CorrelationID: correlationID,
		CausationID: spec.transaction.ID(), Payload: encoded,
		OccurredAt: occurredAt, CreatedAt: occurredAt,
	})
}

func normalizeCommand(command ProcessWagerCommand) ProcessWagerCommand {
	command.ProviderID = strings.TrimSpace(command.ProviderID)
	command.ExternalTransactionID = strings.TrimSpace(command.ExternalTransactionID)
	command.IdempotencyKey = strings.TrimSpace(command.IdempotencyKey)
	command.WalletID = strings.ToLower(strings.TrimSpace(command.WalletID))
	command.PlayerID = strings.ToLower(strings.TrimSpace(command.PlayerID))
	command.RoundID = strings.TrimSpace(command.RoundID)
	command.GameID = strings.TrimSpace(command.GameID)
	command.Kind = wager.Kind(strings.ToUpper(strings.TrimSpace(string(command.Kind))))
	command.ReferenceExternalTransactionID = strings.TrimSpace(command.ReferenceExternalTransactionID)
	command.CorrelationID = strings.TrimSpace(command.CorrelationID)
	return command
}

func payloadHash(command ProcessWagerCommand) (string, error) {
	minor, err := command.Amount.MinorUnits()
	if err != nil {
		return "", err
	}
	currency, err := command.Amount.Currency()
	if err != nil {
		return "", err
	}
	canonical := struct {
		Version                        uint8          `json:"version"`
		ProviderID                     string         `json:"providerId"`
		ExternalTransactionID          string         `json:"externalTransactionId"`
		WalletID                       string         `json:"walletId"`
		PlayerID                       string         `json:"playerId"`
		RoundID                        string         `json:"roundId"`
		GameID                         string         `json:"gameId"`
		Kind                           wager.Kind     `json:"kind"`
		AmountMinor                    int64          `json:"amountMinor"`
		Currency                       money.Currency `json:"currency"`
		ReferenceExternalTransactionID string         `json:"referenceExternalTransactionId"`
	}{1, command.ProviderID, command.ExternalTransactionID, command.WalletID,
		command.PlayerID, command.RoundID, command.GameID, command.Kind, minor,
		currency, command.ReferenceExternalTransactionID}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("encode canonical wager payload: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func isReferencePending(reference *wager.WagerTransaction) bool {
	return reference != nil && (reference.Status() == wager.Pending || reference.Status() == wager.PendingReference)
}

func referenceFailureCode(err error) wager.FailureCode {
	switch {
	case errors.Is(err, domain.ErrReferenceNotEligible):
		return wager.FailureReferenceNotEligible
	case errors.Is(err, domain.ErrReferenceMismatch), errors.Is(err, domain.ErrReferenceAmountMismatch):
		return wager.FailureInvalidOperation
	default:
		return wager.FailureReferenceNotEligible
	}
}

func isBusinessRejection(err error) bool {
	return errors.Is(err, domain.ErrInsufficientFunds) || errors.Is(err, domain.ErrReversalInsufficientFunds) ||
		errors.Is(err, domain.ErrOverflow) || errors.Is(err, domain.ErrReferenceNotEligible) ||
		errors.Is(err, domain.ErrReferenceMismatch) || errors.Is(err, domain.ErrReferenceAmountMismatch)
}

func failureCode(err error) wager.FailureCode {
	switch {
	case errors.Is(err, domain.ErrInsufficientFunds):
		if errors.Is(err, domain.ErrReversalInsufficientFunds) {
			return wager.FailureReversalInsufficientFunds
		}
		return wager.FailureInsufficientFunds
	case errors.Is(err, domain.ErrReferenceNotEligible), errors.Is(err, domain.ErrReferenceMismatch),
		errors.Is(err, domain.ErrReferenceAmountMismatch):
		return referenceFailureCode(err)
	default:
		return wager.FailureInvalidOperation
	}
}

func (s *FinancialService) timestamp() time.Time {
	return s.now().UTC().Truncate(time.Microsecond)
}

func (s *FinancialService) validate() error {
	if s == nil || s.unitOfWork == nil || s.newID == nil || s.now == nil {
		return errors.New("financial service dependencies are required")
	}
	return nil
}

func newUUID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate UUID: %w", err)
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16]), nil
}
