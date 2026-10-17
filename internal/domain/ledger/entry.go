package ledger

import (
	"fmt"
	"time"

	"github.com/kassiobuck/backend-challenge-go/internal/domain"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/identity"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/money"
)

type Direction string

const (
	Debit  Direction = "DEBIT"
	Credit Direction = "CREDIT"
)

type WalletLedgerEntry struct {
	id            string
	walletID      string
	transactionID string
	direction     Direction
	amount        money.Money
	balanceBefore money.Money
	balanceAfter  money.Money
	createdAt     time.Time
}

func NewEntry(
	id, walletID, transactionID string,
	direction Direction,
	amount, balanceBefore, balanceAfter money.Money,
	createdAt time.Time,
) (WalletLedgerEntry, error) {
	if identity.Validate(id) != nil || identity.Validate(walletID) != nil || identity.Validate(transactionID) != nil {
		return WalletLedgerEntry{}, domain.ErrInvalidLedgerEntry
	}
	if direction != Debit && direction != Credit {
		return WalletLedgerEntry{}, domain.ErrInvalidLedgerEntry
	}
	if !amount.IsValid() || !amount.IsPositive() || !balanceBefore.IsValid() || !balanceAfter.IsValid() {
		return WalletLedgerEntry{}, domain.ErrInvalidLedgerEntry
	}
	currency, _ := amount.Currency()
	beforeCurrency, _ := balanceBefore.Currency()
	afterCurrency, _ := balanceAfter.Currency()
	if currency != beforeCurrency || currency != afterCurrency {
		return WalletLedgerEntry{}, domain.ErrCurrencyMismatch
	}

	expectedAfter := balanceBefore
	var err error
	if direction == Debit {
		expectedAfter, err = balanceBefore.Sub(amount)
	} else {
		expectedAfter, err = balanceBefore.Add(amount)
	}
	if err != nil {
		return WalletLedgerEntry{}, err
	}
	if expectedAfter.IsNegative() || expectedAfter != balanceAfter {
		return WalletLedgerEntry{}, fmt.Errorf("%w: balanceAfter does not match direction and amount", domain.ErrInvalidLedgerEntry)
	}
	if createdAt.IsZero() {
		return WalletLedgerEntry{}, domain.ErrInvalidTimestamp
	}
	return WalletLedgerEntry{
		id:            id,
		walletID:      walletID,
		transactionID: transactionID,
		direction:     direction,
		amount:        amount,
		balanceBefore: balanceBefore,
		balanceAfter:  balanceAfter,
		createdAt:     createdAt.UTC(),
	}, nil
}

func (e WalletLedgerEntry) ID() string                 { return e.id }
func (e WalletLedgerEntry) WalletID() string           { return e.walletID }
func (e WalletLedgerEntry) TransactionID() string      { return e.transactionID }
func (e WalletLedgerEntry) Direction() Direction       { return e.direction }
func (e WalletLedgerEntry) Amount() money.Money        { return e.amount }
func (e WalletLedgerEntry) BalanceBefore() money.Money { return e.balanceBefore }
func (e WalletLedgerEntry) BalanceAfter() money.Money  { return e.balanceAfter }
func (e WalletLedgerEntry) CreatedAt() time.Time       { return e.createdAt }
