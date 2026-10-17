package wallet

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/kassiobuck/backend-challenge-go/internal/domain"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/identity"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/money"
)

const (
	testWalletID = "0192f291-27dd-7d3f-8071-5f8685deef37"
	testPlayerID = "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"
)

func TestOpenAndRehydrateWallet(t *testing.T) {
	initial, _ := money.ParseNonNegativeAmount("100.00", money.BRL)
	createdAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.FixedZone("test", 3600))
	w, err := Open(testWalletID, testPlayerID, initial, createdAt)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if w.Version() != 1 || w.Balance().String() != "100.00" {
		t.Fatalf("wallet = balance %s, version %d; want 100.00, 1", w.Balance(), w.Version())
	}
	if w.CreatedAt().Location() != time.UTC {
		t.Fatalf("CreatedAt location = %v, want UTC", w.CreatedAt().Location())
	}

	restored, err := Rehydrate(w.ID(), w.PlayerID(), w.Currency(), w.Balance(), w.Version(), w.CreatedAt(), w.UpdatedAt())
	if err != nil {
		t.Fatalf("Rehydrate() error = %v", err)
	}
	if restored.Version() != w.Version() || restored.Balance() != w.Balance() {
		t.Fatalf("rehydrated wallet = %#v, original = %#v", restored, w)
	}
}

func TestOpenWalletRejectsInvalidValues(t *testing.T) {
	balance, _ := money.ParseAmount("-0.01", money.BRL)
	zero, _ := money.Zero(money.BRL)
	tests := []struct {
		name    string
		id      string
		player  string
		balance money.Money
		now     time.Time
		want    error
	}{
		{name: "invalid wallet id", id: "bad", player: testPlayerID, balance: zero, now: time.Now(), want: domain.ErrInvalidWallet},
		{name: "invalid player id", id: testWalletID, player: "bad", balance: zero, now: time.Now(), want: domain.ErrInvalidWallet},
		{name: "negative balance", id: testWalletID, player: testPlayerID, balance: balance, now: time.Now(), want: domain.ErrInvalidWallet},
		{name: "zero time", id: testWalletID, player: testPlayerID, balance: zero, want: domain.ErrInvalidTimestamp},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Open(test.id, test.player, test.balance, test.now)
			if !errors.Is(err, test.want) {
				t.Fatalf("Open() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestWalletDebitCreditAndVersion(t *testing.T) {
	now := time.Now().UTC()
	initial, _ := money.ParseNonNegativeAmount("100.00", money.BRL)
	amount, _ := money.ParseNonNegativeAmount("80.00", money.BRL)
	w, _ := Open(testWalletID, testPlayerID, initial, now)

	change, err := w.Debit(amount, now.Add(time.Second))
	if err != nil {
		t.Fatalf("Debit() error = %v", err)
	}
	if change.Before().String() != "100.00" || change.After().String() != "20.00" || change.Version() != 2 {
		t.Fatalf("debit change = %s -> %s v%d", change.Before(), change.After(), change.Version())
	}
	change, err = w.Credit(amount, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("Credit() error = %v", err)
	}
	if change.After().String() != "100.00" || w.Version() != 3 {
		t.Fatalf("credit result = %s, wallet version %d", change.After(), w.Version())
	}
}

func TestWalletDebitCreditRejectInvalidMovements(t *testing.T) {
	now := time.Now().UTC()
	initial, _ := money.ParseNonNegativeAmount("10.00", money.BRL)
	w, _ := Open(testWalletID, testPlayerID, initial, now)
	zero, _ := money.Zero(money.BRL)
	negative, _ := money.ParseAmount("-1.00", money.BRL)
	usd, _ := money.ParseNonNegativeAmount("1.00", money.USD)

	tests := []struct {
		name string
		run  func() error
		want error
	}{
		{name: "debit insufficient", run: func() error { _, err := w.Debit(mustMoney(t, "11.00", money.BRL), now.Add(time.Second)); return err }, want: domain.ErrInsufficientFunds},
		{name: "reversal debit insufficient", run: func() error {
			_, err := w.DebitForReversal(mustMoney(t, "11.00", money.BRL), now.Add(time.Second))
			return err
		}, want: domain.ErrReversalInsufficientFunds},
		{name: "debit zero", run: func() error { _, err := w.Debit(zero, now.Add(time.Second)); return err }, want: domain.ErrInvalidMoney},
		{name: "credit negative", run: func() error { _, err := w.Credit(negative, now.Add(time.Second)); return err }, want: domain.ErrInvalidMoney},
		{name: "credit currency mismatch", run: func() error { _, err := w.Credit(usd, now.Add(time.Second)); return err }, want: domain.ErrCurrencyMismatch},
		{name: "time moved backwards", run: func() error { _, err := w.Credit(mustMoney(t, "1.00", money.BRL), now.Add(-time.Second)); return err }, want: domain.ErrInvalidTimestamp},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); !errors.Is(err, test.want) {
				t.Fatalf("movement error = %v, want %v", err, test.want)
			}
		})
	}
	if w.Balance().String() != "10.00" || w.Version() != 1 {
		t.Fatalf("failed movement mutated wallet: balance=%s version=%d", w.Balance(), w.Version())
	}
}

func TestWalletCreditOverflowAndVersionOverflow(t *testing.T) {
	now := time.Now().UTC()
	max, _ := money.NewMinor(math.MaxInt64, money.BRL)
	w, _ := Open(testWalletID, testPlayerID, max, now)
	if _, err := w.Credit(mustMoney(t, "0.01", money.BRL), now.Add(time.Second)); !errors.Is(err, domain.ErrOverflow) {
		t.Fatalf("Credit overflow error = %v, want ErrOverflow", err)
	}

	w, err := Rehydrate(testWalletID, testPlayerID, money.BRL, max, math.MaxUint64, now, now)
	if err != nil {
		t.Fatalf("Rehydrate() error = %v", err)
	}
	if _, err := w.Credit(mustMoney(t, "0.01", money.BRL), now); !errors.Is(err, domain.ErrVersionOverflow) {
		t.Fatalf("Credit version overflow error = %v, want ErrVersionOverflow", err)
	}
}

func TestRehydrateRejectsInvalidPersistedState(t *testing.T) {
	now := time.Now().UTC()
	zero, _ := money.Zero(money.BRL)
	negative, _ := money.ParseAmount("-1.00", money.BRL)
	tests := []struct {
		name    string
		id      string
		balance money.Money
		version uint64
		created time.Time
		updated time.Time
	}{
		{name: "bad id", id: "bad", balance: zero, version: 1, created: now, updated: now},
		{name: "negative balance", id: testWalletID, balance: negative, version: 1, created: now, updated: now},
		{name: "zero version", id: testWalletID, balance: zero, created: now, updated: now},
		{name: "updated before created", id: testWalletID, balance: zero, version: 1, created: now, updated: now.Add(-time.Second)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Rehydrate(test.id, testPlayerID, money.BRL, test.balance, test.version, test.created, test.updated)
			if !errors.Is(err, domain.ErrInvalidWalletState) {
				t.Fatalf("Rehydrate() error = %v, want ErrInvalidWalletState", err)
			}
		})
	}
}

func TestIdentityValidation(t *testing.T) {
	if err := identity.Validate(testWalletID); err != nil {
		t.Fatalf("valid UUID rejected: %v", err)
	}
}

func mustMoney(t *testing.T, value string, currency money.Currency) money.Money {
	t.Helper()
	result, err := money.ParseNonNegativeAmount(value, currency)
	if err != nil {
		t.Fatalf("parse money %q: %v", value, err)
	}
	return result
}
