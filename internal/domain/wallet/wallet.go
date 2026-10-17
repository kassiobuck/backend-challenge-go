package wallet

import (
	"fmt"
	"time"

	"github.com/kassiobuck/backend-challenge-go/internal/domain"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/identity"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/money"
)

type Wallet struct {
	id        string
	playerID  string
	currency  money.Currency
	balance   money.Money
	version   uint64
	createdAt time.Time
	updatedAt time.Time
}

type BalanceChange struct {
	before  money.Money
	after   money.Money
	version uint64
}

func Open(id, playerID string, initialBalance money.Money, now time.Time) (*Wallet, error) {
	if err := identity.Validate(id); err != nil {
		return nil, fmt.Errorf("%w: wallet id: %v", domain.ErrInvalidWallet, err)
	}
	if err := identity.Validate(playerID); err != nil {
		return nil, fmt.Errorf("%w: player id: %v", domain.ErrInvalidWallet, err)
	}
	if !initialBalance.IsValid() || initialBalance.IsNegative() {
		return nil, fmt.Errorf("%w: initial balance must be valid and non-negative", domain.ErrInvalidWallet)
	}
	if now.IsZero() {
		return nil, domain.ErrInvalidTimestamp
	}
	currency, _ := initialBalance.Currency()
	utcNow := now.UTC()
	return &Wallet{
		id:        id,
		playerID:  playerID,
		currency:  currency,
		balance:   initialBalance,
		version:   1,
		createdAt: utcNow,
		updatedAt: utcNow,
	}, nil
}

func Rehydrate(id, playerID string, currency money.Currency, balance money.Money, version uint64, createdAt, updatedAt time.Time) (*Wallet, error) {
	if err := identity.Validate(id); err != nil {
		return nil, fmt.Errorf("%w: wallet id: %v", domain.ErrInvalidWalletState, err)
	}
	if err := identity.Validate(playerID); err != nil {
		return nil, fmt.Errorf("%w: player id: %v", domain.ErrInvalidWalletState, err)
	}
	balanceCurrency, err := balance.Currency()
	if err != nil || balanceCurrency != currency || balance.IsNegative() {
		return nil, fmt.Errorf("%w: invalid balance or currency", domain.ErrInvalidWalletState)
	}
	if version == 0 || createdAt.IsZero() || updatedAt.IsZero() || updatedAt.Before(createdAt) {
		return nil, domain.ErrInvalidWalletState
	}
	return &Wallet{
		id:        id,
		playerID:  playerID,
		currency:  currency,
		balance:   balance,
		version:   version,
		createdAt: createdAt.UTC(),
		updatedAt: updatedAt.UTC(),
	}, nil
}

func (w *Wallet) Debit(amount money.Money, now time.Time) (BalanceChange, error) {
	return w.debit(amount, now, domain.ErrInsufficientFunds)
}

func (w *Wallet) DebitForReversal(amount money.Money, now time.Time) (BalanceChange, error) {
	return w.debit(amount, now, domain.ErrReversalInsufficientFunds)
}

func (w *Wallet) debit(amount money.Money, now time.Time, insufficientFunds error) (BalanceChange, error) {
	if err := w.validateOperation(amount, now); err != nil {
		return BalanceChange{}, err
	}
	next, err := w.balance.Sub(amount)
	if err != nil {
		return BalanceChange{}, err
	}
	if next.IsNegative() {
		return BalanceChange{}, insufficientFunds
	}
	return w.commitBalance(next, now)
}

func (w *Wallet) Credit(amount money.Money, now time.Time) (BalanceChange, error) {
	if err := w.validateOperation(amount, now); err != nil {
		return BalanceChange{}, err
	}
	next, err := w.balance.Add(amount)
	if err != nil {
		return BalanceChange{}, err
	}
	return w.commitBalance(next, now)
}

func (w *Wallet) commitBalance(next money.Money, now time.Time) (BalanceChange, error) {
	if w.version == ^uint64(0) {
		return BalanceChange{}, domain.ErrVersionOverflow
	}
	before := w.balance
	w.balance = next
	w.version++
	w.updatedAt = now.UTC()
	return BalanceChange{before: before, after: next, version: w.version}, nil
}

func (w *Wallet) validateOperation(amount money.Money, now time.Time) error {
	if w == nil {
		return domain.ErrInvalidWalletState
	}
	if !amount.IsValid() || !amount.IsPositive() {
		return fmt.Errorf("%w: movement amount must be positive", domain.ErrInvalidMoney)
	}
	currency, _ := amount.Currency()
	if currency != w.currency {
		return domain.ErrCurrencyMismatch
	}
	if now.IsZero() || now.Before(w.updatedAt) {
		return domain.ErrInvalidTimestamp
	}
	if w.version == ^uint64(0) {
		return domain.ErrVersionOverflow
	}
	return nil
}

func (w *Wallet) ID() string                { return w.id }
func (w *Wallet) PlayerID() string          { return w.playerID }
func (w *Wallet) Currency() money.Currency  { return w.currency }
func (w *Wallet) Balance() money.Money      { return w.balance }
func (w *Wallet) Version() uint64           { return w.version }
func (w *Wallet) CreatedAt() time.Time      { return w.createdAt }
func (w *Wallet) UpdatedAt() time.Time      { return w.updatedAt }
func (c BalanceChange) Before() money.Money { return c.before }
func (c BalanceChange) After() money.Money  { return c.after }
func (c BalanceChange) Version() uint64     { return c.version }
