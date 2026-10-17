package ledger

import (
	"errors"
	"testing"
	"time"

	"github.com/kassiobuck/backend-challenge-go/internal/domain"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/money"
)

const (
	ledgerID       = "0192f291-27dd-7d3f-8071-5f8685deef37"
	ledgerWalletID = "0192f292-27dd-7d3f-8071-5f8685deef37"
	ledgerTxnID    = "0192f293-27dd-7d3f-8071-5f8685deef37"
)

func TestNewLedgerEntry(t *testing.T) {
	entry, err := NewEntry(
		ledgerID, ledgerWalletID, ledgerTxnID, Debit,
		mustMoney(t, "25.00"), mustMoney(t, "100.00"), mustMoney(t, "75.00"),
		time.Now(),
	)
	if err != nil {
		t.Fatalf("NewEntry() error = %v", err)
	}
	if entry.Direction() != Debit || entry.BalanceAfter().String() != "75.00" {
		t.Fatalf("entry direction/balance = %s/%s", entry.Direction(), entry.BalanceAfter())
	}
}

func TestNewLedgerEntryRejectsInvalidValues(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name      string
		direction Direction
		amount    money.Money
		before    money.Money
		after     money.Money
		want      error
	}{
		{name: "wrong balance equation", direction: Debit, amount: mustMoney(t, "25.00"), before: mustMoney(t, "100.00"), after: mustMoney(t, "80.00"), want: domain.ErrInvalidLedgerEntry},
		{name: "negative debit balance", direction: Debit, amount: mustMoney(t, "25.00"), before: mustMoney(t, "10.00"), after: mustMoney(t, "-15.00"), want: domain.ErrInvalidLedgerEntry},
		{name: "zero amount", direction: Credit, amount: mustMoney(t, "0.00"), before: mustMoney(t, "10.00"), after: mustMoney(t, "10.00"), want: domain.ErrInvalidLedgerEntry},
		{name: "invalid direction", direction: "UNKNOWN", amount: mustMoney(t, "1.00"), before: mustMoney(t, "10.00"), after: mustMoney(t, "11.00"), want: domain.ErrInvalidLedgerEntry},
		{name: "zero timestamp", direction: Credit, amount: mustMoney(t, "1.00"), before: mustMoney(t, "10.00"), after: mustMoney(t, "11.00"), want: domain.ErrInvalidTimestamp},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			at := now
			if test.name == "zero timestamp" {
				at = time.Time{}
			}
			_, err := NewEntry(ledgerID, ledgerWalletID, ledgerTxnID, test.direction, test.amount, test.before, test.after, at)
			if !errors.Is(err, test.want) {
				t.Fatalf("NewEntry() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestNewLedgerEntryRejectsCurrencyMismatch(t *testing.T) {
	_, err := NewEntry(
		ledgerID, ledgerWalletID, ledgerTxnID, Credit,
		mustMoney(t, "1.00"), mustMoney(t, "10.00"), mustMoneyCurrency(t, "11.00", money.USD),
		time.Now(),
	)
	if !errors.Is(err, domain.ErrCurrencyMismatch) {
		t.Fatalf("NewEntry() error = %v, want ErrCurrencyMismatch", err)
	}
}

func mustMoney(t *testing.T, amount string) money.Money {
	t.Helper()
	return mustMoneyCurrency(t, amount, money.BRL)
}

func mustMoneyCurrency(t *testing.T, amount string, currency money.Currency) money.Money {
	t.Helper()
	value, err := money.ParseAmount(amount, currency)
	if err != nil {
		t.Fatalf("ParseAmount(%q): %v", amount, err)
	}
	return value
}
