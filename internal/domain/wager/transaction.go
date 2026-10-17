package wager

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/kassiobuck/backend-challenge-go/internal/domain"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/identity"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/ledger"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/money"
)

type Kind string

const (
	Opening  Kind = "OPENING"
	Bet      Kind = "BET"
	Win      Kind = "WIN"
	Loss     Kind = "LOSS"
	Refund   Kind = "REFUND"
	Rollback Kind = "ROLLBACK"
)

type Status string

const (
	Pending          Status = "PENDING"
	PendingReference Status = "PENDING_REFERENCE"
	Processed        Status = "PROCESSED"
	Rejected         Status = "REJECTED"
	Failed           Status = "FAILED"
)

type FailureCode string

const (
	FailureInsufficientFunds         FailureCode = "INSUFFICIENT_FUNDS"
	FailureReversalInsufficientFunds FailureCode = "REVERSAL_INSUFFICIENT_FUNDS"
	FailureReferenceNotFound         FailureCode = "REFERENCE_NOT_FOUND"
	FailureReferenceNotEligible      FailureCode = "REFERENCE_NOT_ELIGIBLE"
	FailureReversalAlreadyApplied    FailureCode = "REVERSAL_ALREADY_APPLIED"
	FailureInvalidOperation          FailureCode = "INVALID_OPERATION"
	FailurePermanentInfrastructure   FailureCode = "PERMANENT_INFRASTRUCTURE"
)

type Origin string

const (
	External Origin = "EXTERNAL"
	Internal Origin = "INTERNAL"
)

type ExternalParams struct {
	ID                             string
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PayloadHash                    string
	WalletID                       string
	PlayerID                       string
	RoundID                        string
	GameID                         string
	Kind                           Kind
	Amount                         money.Money
	ReferenceExternalTransactionID string
	CreatedAt                      time.Time
}

type OpeningParams struct {
	ID        string
	WalletID  string
	PlayerID  string
	Amount    money.Money
	CreatedAt time.Time
}

type Snapshot struct {
	Origin                         Origin
	ID                             string
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PayloadHash                    string
	WalletID                       string
	PlayerID                       string
	RoundID                        string
	GameID                         string
	Kind                           Kind
	Amount                         money.Money
	ReferenceExternalTransactionID string
	ResolvedReferenceID            string
	Status                         Status
	FailureCode                    FailureCode
	ResultBalance                  money.Money
	HasResultBalance               bool
	CreatedAt                      time.Time
	UpdatedAt                      time.Time
}

type WagerTransaction struct {
	snapshot Snapshot
}

type Direction = ledger.Direction

const (
	Debit  = ledger.Debit
	Credit = ledger.Credit
)

func NewExternal(params ExternalParams) (*WagerTransaction, error) {
	if err := validateExternalParams(params); err != nil {
		return nil, err
	}
	return &WagerTransaction{snapshot: Snapshot{
		Origin:                         External,
		ID:                             params.ID,
		ProviderID:                     params.ProviderID,
		ExternalTransactionID:          params.ExternalTransactionID,
		IdempotencyKey:                 params.IdempotencyKey,
		PayloadHash:                    strings.ToLower(params.PayloadHash),
		WalletID:                       params.WalletID,
		PlayerID:                       params.PlayerID,
		RoundID:                        params.RoundID,
		GameID:                         params.GameID,
		Kind:                           params.Kind,
		Amount:                         params.Amount,
		ReferenceExternalTransactionID: params.ReferenceExternalTransactionID,
		Status:                         Pending,
		CreatedAt:                      params.CreatedAt.UTC(),
		UpdatedAt:                      params.CreatedAt.UTC(),
	}}, nil
}

func NewOpening(params OpeningParams) (*WagerTransaction, error) {
	if identity.Validate(params.ID) != nil || identity.Validate(params.WalletID) != nil || identity.Validate(params.PlayerID) != nil {
		return nil, domain.ErrInvalidWager
	}
	if !params.Amount.IsValid() || !params.Amount.IsPositive() {
		return nil, fmt.Errorf("%w: OPENING amount must be positive", domain.ErrInvalidWager)
	}
	if params.CreatedAt.IsZero() {
		return nil, domain.ErrInvalidTimestamp
	}
	now := params.CreatedAt.UTC()
	return &WagerTransaction{snapshot: Snapshot{
		Origin:           Internal,
		ID:               params.ID,
		WalletID:         params.WalletID,
		PlayerID:         params.PlayerID,
		Kind:             Opening,
		Amount:           params.Amount,
		Status:           Processed,
		ResultBalance:    params.Amount,
		HasResultBalance: true,
		CreatedAt:        now,
		UpdatedAt:        now,
	}}, nil
}

func Rehydrate(snapshot Snapshot) (*WagerTransaction, error) {
	if snapshot.Origin == External {
		if snapshot.CreatedAt.IsZero() {
			return nil, domain.ErrInvalidTimestamp
		}
		params := ExternalParams{
			ID:                             snapshot.ID,
			ProviderID:                     snapshot.ProviderID,
			ExternalTransactionID:          snapshot.ExternalTransactionID,
			IdempotencyKey:                 snapshot.IdempotencyKey,
			PayloadHash:                    snapshot.PayloadHash,
			WalletID:                       snapshot.WalletID,
			PlayerID:                       snapshot.PlayerID,
			RoundID:                        snapshot.RoundID,
			GameID:                         snapshot.GameID,
			Kind:                           snapshot.Kind,
			Amount:                         snapshot.Amount,
			ReferenceExternalTransactionID: snapshot.ReferenceExternalTransactionID,
			CreatedAt:                      snapshot.CreatedAt,
		}
		transaction, err := NewExternal(params)
		if err != nil {
			return nil, err
		}
		if err := validateSnapshotState(snapshot); err != nil {
			return nil, err
		}
		snapshot.PayloadHash = strings.ToLower(snapshot.PayloadHash)
		snapshot.CreatedAt = snapshot.CreatedAt.UTC()
		snapshot.UpdatedAt = snapshot.UpdatedAt.UTC()
		transaction.snapshot = snapshot
		return transaction, nil
	}
	if snapshot.Origin != Internal || snapshot.Kind != Opening ||
		identity.Validate(snapshot.ID) != nil || identity.Validate(snapshot.WalletID) != nil ||
		identity.Validate(snapshot.PlayerID) != nil || !snapshot.Amount.IsValid() ||
		!snapshot.Amount.IsPositive() || snapshot.Status != Processed ||
		!snapshot.HasResultBalance || snapshot.ResultBalance != snapshot.Amount ||
		snapshot.CreatedAt.IsZero() || snapshot.UpdatedAt.IsZero() ||
		snapshot.UpdatedAt.Before(snapshot.CreatedAt) {
		return nil, domain.ErrInvalidWager
	}
	snapshot.CreatedAt = snapshot.CreatedAt.UTC()
	snapshot.UpdatedAt = snapshot.UpdatedAt.UTC()
	return &WagerTransaction{snapshot: snapshot}, nil
}

func (w *WagerTransaction) WaitForReference(at time.Time) error {
	if w == nil || w.snapshot.Status != Pending || !hasReferenceDependency(w.snapshot.Kind, w.snapshot.ReferenceExternalTransactionID) {
		return domain.ErrInvalidTransition
	}
	if err := w.validateTransitionTime(at); err != nil {
		return err
	}
	w.snapshot.Status = PendingReference
	w.snapshot.UpdatedAt = at.UTC()
	return nil
}

func (w *WagerTransaction) Process(
	balanceBefore, resultingBalance money.Money,
	reference *WagerTransaction,
	at time.Time,
) error {
	if w == nil || (w.snapshot.Status != Pending && w.snapshot.Status != PendingReference) {
		return domain.ErrInvalidTransition
	}
	if !balanceBefore.IsValid() || balanceBefore.IsNegative() ||
		!resultingBalance.IsValid() || resultingBalance.IsNegative() {
		return domain.ErrInvalidWager
	}
	beforeCurrency, _ := balanceBefore.Currency()
	balanceCurrency, _ := resultingBalance.Currency()
	amountCurrency, _ := w.snapshot.Amount.Currency()
	if beforeCurrency != amountCurrency || balanceCurrency != amountCurrency {
		return domain.ErrCurrencyMismatch
	}
	direction, moves, err := w.Movement(reference)
	if err != nil {
		return err
	}
	expectedBalance := balanceBefore
	if moves {
		if direction == Debit {
			expectedBalance, err = balanceBefore.Sub(w.snapshot.Amount)
			if err != nil {
				return err
			}
			if expectedBalance.IsNegative() {
				if w.snapshot.Kind == Rollback {
					return domain.ErrReversalInsufficientFunds
				}
				return domain.ErrInsufficientFunds
			}
		} else {
			expectedBalance, err = balanceBefore.Add(w.snapshot.Amount)
			if err != nil {
				return err
			}
		}
	}
	if expectedBalance != resultingBalance {
		return fmt.Errorf("%w: resulting balance does not match transaction movement", domain.ErrInvalidWager)
	}
	if err := w.validateTransitionTime(at); err != nil {
		return err
	}
	w.snapshot.Status = Processed
	w.snapshot.ResultBalance = resultingBalance
	w.snapshot.HasResultBalance = true
	w.snapshot.UpdatedAt = at.UTC()
	if reference != nil {
		w.snapshot.ResolvedReferenceID = reference.ID()
	}
	return nil
}

func (w *WagerTransaction) Reject(code FailureCode, at time.Time) error {
	if w == nil || (w.snapshot.Status != Pending && w.snapshot.Status != PendingReference) {
		return domain.ErrInvalidTransition
	}
	if !validFailureCode(code) {
		return domain.ErrInvalidFailureCode
	}
	if err := w.validateTransitionTime(at); err != nil {
		return err
	}
	w.snapshot.Status = Rejected
	w.snapshot.FailureCode = code
	w.snapshot.UpdatedAt = at.UTC()
	return nil
}

func (w *WagerTransaction) Fail(code FailureCode, at time.Time) error {
	if w == nil || (w.snapshot.Status != Pending && w.snapshot.Status != PendingReference) {
		return domain.ErrInvalidTransition
	}
	if !validFailureCode(code) {
		return domain.ErrInvalidFailureCode
	}
	if err := w.validateTransitionTime(at); err != nil {
		return err
	}
	w.snapshot.Status = Failed
	w.snapshot.FailureCode = code
	w.snapshot.UpdatedAt = at.UTC()
	return nil
}

func (w *WagerTransaction) ValidateReference(reference *WagerTransaction) error {
	if w == nil {
		return domain.ErrInvalidWager
	}
	kind := w.snapshot.Kind
	if kind == Win && w.snapshot.ReferenceExternalTransactionID == "" && reference == nil {
		return nil
	}
	if !requiresReference(kind) && w.snapshot.ReferenceExternalTransactionID == "" {
		if reference != nil {
			return domain.ErrReferenceMismatch
		}
		return nil
	}
	if reference == nil {
		return domain.ErrReferencePending
	}
	if w.snapshot.ReferenceExternalTransactionID == "" ||
		reference.ExternalTransactionID() != w.snapshot.ReferenceExternalTransactionID {
		return domain.ErrReferenceMismatch
	}
	if reference.Status() == Pending || reference.Status() == PendingReference {
		return domain.ErrReferencePending
	}
	if reference.Status() != Processed {
		return domain.ErrReferenceNotEligible
	}
	switch kind {
	case Win, Refund:
		if reference.Kind() != Bet {
			return domain.ErrReferenceNotEligible
		}
	case Rollback:
		if reference.Kind() != Bet && reference.Kind() != Win && reference.Kind() != Refund {
			return domain.ErrReferenceNotEligible
		}
	default:
		return domain.ErrReferenceNotEligible
	}
	referenceCurrency, _ := reference.Amount().Currency()
	amountCurrency, _ := w.snapshot.Amount.Currency()
	if referenceCurrency != amountCurrency {
		return domain.ErrCurrencyMismatch
	}
	if reference.Origin() != External ||
		reference.ProviderID() != w.snapshot.ProviderID ||
		reference.PlayerID() != w.snapshot.PlayerID ||
		reference.WalletID() != w.snapshot.WalletID ||
		reference.RoundID() != w.snapshot.RoundID {
		return domain.ErrReferenceMismatch
	}
	amountComparison, err := reference.Amount().Compare(w.snapshot.Amount)
	if err != nil {
		return err
	}
	if amountComparison != 0 {
		return domain.ErrReferenceAmountMismatch
	}

	return nil
}

func (w *WagerTransaction) Movement(reference *WagerTransaction) (Direction, bool, error) {
	if w == nil {
		return "", false, domain.ErrInvalidWager
	}
	if w.snapshot.Kind == Opening {
		if w.snapshot.Status != Processed {
			return "", false, domain.ErrInvalidTransition
		}
	} else if w.snapshot.Status != Pending && w.snapshot.Status != PendingReference {
		return "", false, domain.ErrInvalidTransition
	}
	if err := w.ValidateReference(reference); err != nil {
		return "", false, err
	}
	switch w.snapshot.Kind {
	case Opening, Win, Refund:
		return Credit, true, nil
	case Bet:
		return Debit, true, nil
	case Loss:
		return "", false, nil
	case Rollback:
		if reference.Kind() == Bet {
			return Credit, true, nil
		}
		return Debit, true, nil
	default:
		return "", false, domain.ErrInvalidWagerKind
	}
}

func (w *WagerTransaction) validateTransitionTime(at time.Time) error {
	if at.IsZero() || at.Before(w.snapshot.UpdatedAt) {
		return domain.ErrInvalidTimestamp
	}
	return nil
}

func validateExternalParams(params ExternalParams) error {
	if identity.Validate(params.ID) != nil || identity.Validate(params.WalletID) != nil || identity.Validate(params.PlayerID) != nil {
		return domain.ErrInvalidWager
	}
	if !nonBlank(params.ProviderID) || !nonBlank(params.ExternalTransactionID) ||
		!nonBlank(params.IdempotencyKey) || !validHash(params.PayloadHash) ||
		!nonBlank(params.RoundID) || !nonBlank(params.GameID) {
		return domain.ErrInvalidWager
	}
	if params.CreatedAt.IsZero() {
		return domain.ErrInvalidTimestamp
	}
	if !params.Amount.IsValid() {
		return domain.ErrInvalidMoney
	}
	switch params.Kind {
	case Bet, Win, Refund, Rollback:
		if !params.Amount.IsPositive() {
			return fmt.Errorf("%w: %s amount must be positive", domain.ErrInvalidWager, params.Kind)
		}
	case Loss:
		if !params.Amount.IsZero() {
			return fmt.Errorf("%w: LOSS amount must be zero", domain.ErrInvalidWager)
		}
	case Opening:
		return domain.ErrInvalidWagerKind
	default:
		return domain.ErrInvalidWagerKind
	}
	switch params.Kind {
	case Refund, Rollback:
		if !nonBlank(params.ReferenceExternalTransactionID) {
			return domain.ErrReferenceRequired
		}
	case Bet, Loss:
		if params.ReferenceExternalTransactionID != "" {
			return domain.ErrInvalidWager
		}
	}
	return nil
}

func validateSnapshotState(snapshot Snapshot) error {
	if snapshot.UpdatedAt.IsZero() || snapshot.UpdatedAt.Before(snapshot.CreatedAt) {
		return domain.ErrInvalidTimestamp
	}
	if snapshot.Status != Pending && snapshot.Status != PendingReference &&
		snapshot.Status != Processed && snapshot.Status != Rejected && snapshot.Status != Failed {
		return domain.ErrInvalidWagerState
	}
	if snapshot.Status == PendingReference && !hasReferenceDependency(snapshot.Kind, snapshot.ReferenceExternalTransactionID) {
		return domain.ErrInvalidWagerState
	}
	if snapshot.Status == Processed {
		if !snapshot.HasResultBalance || !snapshot.ResultBalance.IsValid() || snapshot.ResultBalance.IsNegative() {
			return domain.ErrInvalidWagerState
		}
		resultCurrency, _ := snapshot.ResultBalance.Currency()
		amountCurrency, _ := snapshot.Amount.Currency()
		if resultCurrency != amountCurrency {
			return domain.ErrCurrencyMismatch
		}
		if hasReferenceDependency(snapshot.Kind, snapshot.ReferenceExternalTransactionID) &&
			identity.Validate(snapshot.ResolvedReferenceID) != nil {
			return domain.ErrInvalidWagerState
		}
	} else if snapshot.HasResultBalance {
		return domain.ErrInvalidWagerState
	}
	if snapshot.Status == Rejected || snapshot.Status == Failed {
		if !validFailureCode(snapshot.FailureCode) {
			return domain.ErrInvalidWagerState
		}
	} else if snapshot.FailureCode != "" {
		return domain.ErrInvalidWagerState
	}
	return nil
}

func requiresReference(kind Kind) bool {
	return kind == Refund || kind == Rollback
}

func hasReferenceDependency(kind Kind, externalReferenceID string) bool {
	return requiresReference(kind) || (kind == Win && nonBlank(externalReferenceID))
}

func nonBlank(value string) bool {
	return strings.TrimSpace(value) != ""
}

func validHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}

func validFailureCode(code FailureCode) bool {
	if len(code) == 0 || len(code) > 64 {
		return false
	}
	for _, char := range code {
		if !((char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '_') {
			return false
		}
	}
	return true
}

func (w *WagerTransaction) Snapshot() Snapshot            { return w.snapshot }
func (w *WagerTransaction) ID() string                    { return w.snapshot.ID }
func (w *WagerTransaction) Origin() Origin                { return w.snapshot.Origin }
func (w *WagerTransaction) ProviderID() string            { return w.snapshot.ProviderID }
func (w *WagerTransaction) ExternalTransactionID() string { return w.snapshot.ExternalTransactionID }
func (w *WagerTransaction) WalletID() string              { return w.snapshot.WalletID }
func (w *WagerTransaction) PlayerID() string              { return w.snapshot.PlayerID }
func (w *WagerTransaction) RoundID() string               { return w.snapshot.RoundID }
func (w *WagerTransaction) Kind() Kind                    { return w.snapshot.Kind }
func (w *WagerTransaction) Amount() money.Money           { return w.snapshot.Amount }
func (w *WagerTransaction) ReferenceExternalTransactionID() string {
	return w.snapshot.ReferenceExternalTransactionID
}
func (w *WagerTransaction) ResolvedReferenceID() string { return w.snapshot.ResolvedReferenceID }
func (w *WagerTransaction) Status() Status              { return w.snapshot.Status }
func (w *WagerTransaction) FailureCode() FailureCode    { return w.snapshot.FailureCode }
func (w *WagerTransaction) ResultBalance() (money.Money, bool) {
	return w.snapshot.ResultBalance, w.snapshot.HasResultBalance
}
func (w *WagerTransaction) CreatedAt() time.Time { return w.snapshot.CreatedAt }
func (w *WagerTransaction) UpdatedAt() time.Time { return w.snapshot.UpdatedAt }
