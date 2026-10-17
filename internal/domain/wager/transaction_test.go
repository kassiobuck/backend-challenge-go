package wager

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kassiobuck/backend-challenge-go/internal/domain"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/money"
)

const (
	wagerID       = "0192f291-27dd-7d3f-8071-5f8685deef37"
	wagerWalletID = "0192f292-27dd-7d3f-8071-5f8685deef37"
	wagerPlayerID = "0192f293-27dd-7d3f-8071-5f8685deef37"
	referenceID   = "0192f294-27dd-7d3f-8071-5f8685deef37"
)

func TestNewExternalTransactionRules(t *testing.T) {
	tests := []struct {
		name      string
		kind      Kind
		amount    string
		ref       string
		wantError error
	}{
		{name: "BET positive", kind: Bet, amount: "10.00"},
		{name: "WIN positive", kind: Win, amount: "10.00"},
		{name: "LOSS zero", kind: Loss, amount: "0.00"},
		{name: "REFUND positive with reference", kind: Refund, amount: "10.00", ref: "bet-1"},
		{name: "ROLLBACK positive with reference", kind: Rollback, amount: "10.00", ref: "bet-1"},
		{name: "BET zero rejected", kind: Bet, amount: "0.00", wantError: domain.ErrInvalidWager},
		{name: "BET negative rejected", kind: Bet, amount: "-1.00", wantError: domain.ErrInvalidWager},
		{name: "WIN zero rejected", kind: Win, amount: "0.00", wantError: domain.ErrInvalidWager},
		{name: "LOSS positive rejected", kind: Loss, amount: "0.01", wantError: domain.ErrInvalidWager},
		{name: "LOSS negative rejected", kind: Loss, amount: "-0.01", wantError: domain.ErrInvalidWager},
		{name: "REFUND requires reference", kind: Refund, amount: "1.00", wantError: domain.ErrReferenceRequired},
		{name: "ROLLBACK requires reference", kind: Rollback, amount: "1.00", wantError: domain.ErrReferenceRequired},
		{name: "OPENING forbidden externally", kind: Opening, amount: "1.00", wantError: domain.ErrInvalidWagerKind},
		{name: "unknown kind", kind: "BONUS", amount: "1.00", wantError: domain.ErrInvalidWagerKind},
		{name: "BET cannot carry reference", kind: Bet, amount: "1.00", ref: "bet-1", wantError: domain.ErrInvalidWager},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			params := validExternalParams(t, test.kind, test.amount, test.ref)
			transaction, err := NewExternal(params)
			if test.wantError != nil {
				if err == nil || !errors.Is(err, test.wantError) {
					t.Fatalf("NewExternal() error = %v, want %v", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewExternal() error = %v", err)
			}
			if transaction.Status() != Pending || transaction.Origin() != External {
				t.Fatalf("new transaction state/origin = %s/%s", transaction.Status(), transaction.Origin())
			}
		})
	}
}

func TestNewExternalValidatesMetadataAndMoney(t *testing.T) {
	tests := []struct {
		name   string
		change func(*ExternalParams)
		want   error
	}{
		{name: "invalid transaction UUID", change: func(p *ExternalParams) { p.ID = "bad" }, want: domain.ErrInvalidWager},
		{name: "invalid wallet UUID", change: func(p *ExternalParams) { p.WalletID = "bad" }, want: domain.ErrInvalidWager},
		{name: "missing provider", change: func(p *ExternalParams) { p.ProviderID = " " }, want: domain.ErrInvalidWager},
		{name: "missing external id", change: func(p *ExternalParams) { p.ExternalTransactionID = "" }, want: domain.ErrInvalidWager},
		{name: "missing idempotency key", change: func(p *ExternalParams) { p.IdempotencyKey = "" }, want: domain.ErrInvalidWager},
		{name: "invalid hash", change: func(p *ExternalParams) { p.PayloadHash = "hash" }, want: domain.ErrInvalidWager},
		{name: "invalid game", change: func(p *ExternalParams) { p.GameID = "" }, want: domain.ErrInvalidWager},
		{name: "zero timestamp", change: func(p *ExternalParams) { p.CreatedAt = time.Time{} }, want: domain.ErrInvalidTimestamp},
		{name: "invalid money zero value", change: func(p *ExternalParams) { p.Amount = money.Money{} }, want: domain.ErrInvalidMoney},
		{name: "WIN cannot reference a non-BET", change: func(p *ExternalParams) { p.ReferenceExternalTransactionID = "optional" }, want: nil},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			params := validExternalParams(t, Bet, "1.00", "")
			if test.name == "WIN cannot reference a non-BET" {
				params = validExternalParams(t, Win, "1.00", "optional")
			}
			test.change(&params)
			_, err := NewExternal(params)
			if test.want == nil {
				if err != nil {
					t.Fatalf("NewExternal() error = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("NewExternal() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestOpeningIsInternalProcessedAndRehydratable(t *testing.T) {
	amount := mustWagerMoney(t, "100.00", money.BRL)
	now := time.Now()
	opening, err := NewOpening(OpeningParams{
		ID:        wagerID,
		WalletID:  wagerWalletID,
		PlayerID:  wagerPlayerID,
		Amount:    amount,
		CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("NewOpening() error = %v", err)
	}
	if opening.Kind() != Opening || opening.Origin() != Internal || opening.Status() != Processed {
		t.Fatalf("opening kind/origin/status = %s/%s/%s", opening.Kind(), opening.Origin(), opening.Status())
	}
	result, hasResult := opening.ResultBalance()
	if !hasResult || result != amount {
		t.Fatalf("opening result balance = %s, has=%t; want %s", result, hasResult, amount)
	}

	restored, err := Rehydrate(opening.Snapshot())
	if err != nil {
		t.Fatalf("Rehydrate(opening) error = %v", err)
	}
	restoredBalance, restoredHasBalance := restored.ResultBalance()
	if restored.Status() != Processed || !restoredHasBalance || restoredBalance != result {
		t.Fatalf("rehydrated opening = %#v", restored.Snapshot())
	}

	_, err = NewOpening(OpeningParams{
		ID: wagerID, WalletID: wagerWalletID, PlayerID: wagerPlayerID,
		Amount: mustWagerMoney(t, "0.00", money.BRL), CreatedAt: now,
	})
	if !errors.Is(err, domain.ErrInvalidWager) {
		t.Fatalf("zero OPENING error = %v, want ErrInvalidWager", err)
	}
}

func TestWaitForReferenceAndProcessTransitions(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name string
		kind Kind
		ref  string
	}{
		{name: "refund", kind: Refund, ref: "bet-1"},
		{name: "rollback", kind: Rollback, ref: "win-1"},
	}
	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			params := validExternalParams(t, test.kind, "10.00", test.ref)
			params.ID = []string{wagerID, referenceID}[i]
			transaction, err := NewExternal(params)
			if err != nil {
				t.Fatalf("NewExternal() error = %v", err)
			}
			if err := transaction.WaitForReference(now.Add(time.Second)); err != nil {
				t.Fatalf("WaitForReference() error = %v", err)
			}
			if transaction.Status() != PendingReference {
				t.Fatalf("status = %s, want PENDING_REFERENCE", transaction.Status())
			}

			reference := processedReference(t, test.ref, Bet, "10.00", now)
			if test.kind == Rollback {
				reference = processedReference(t, test.ref, Win, "10.00", now)
			}
			before, after := "100.00", "110.00"
			if test.kind == Rollback {
				before, after = "100.00", "90.00"
			}
			if err := transaction.Process(
				mustWagerMoney(t, before, money.BRL),
				mustWagerMoney(t, after, money.BRL),
				reference,
				now.Add(2*time.Second),
			); err != nil {
				t.Fatalf("Process() error = %v", err)
			}
			if transaction.Status() != Processed || transaction.ResolvedReferenceID() != reference.ID() {
				t.Fatalf("processed status/reference = %s/%s", transaction.Status(), transaction.ResolvedReferenceID())
			}
		})
	}
}

func TestPendingCanProcessRejectOrFailAndTerminalStatesDoNotTransition(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name       string
		transition func(*WagerTransaction) error
		want       Status
	}{
		{
			name: "process",
			transition: func(transaction *WagerTransaction) error {
				return transaction.Process(
					mustWagerMoney(t, "100.00", money.BRL),
					mustWagerMoney(t, "90.00", money.BRL),
					nil,
					now.Add(time.Second),
				)
			},
			want: Processed,
		},
		{
			name: "reject",
			transition: func(transaction *WagerTransaction) error {
				return transaction.Reject(FailureInsufficientFunds, now.Add(time.Second))
			},
			want: Rejected,
		},
		{
			name: "fail",
			transition: func(transaction *WagerTransaction) error {
				return transaction.Fail(FailurePermanentInfrastructure, now.Add(time.Second))
			},
			want: Failed,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transaction := newWager(t, Bet, "10.00", "", now)
			if err := test.transition(transaction); err != nil {
				t.Fatalf("transition error = %v", err)
			}
			if transaction.Status() != test.want {
				t.Fatalf("status = %s, want %s", transaction.Status(), test.want)
			}
			if err := transaction.Reject(FailureInvalidOperation, now.Add(2*time.Second)); !errors.Is(err, domain.ErrInvalidTransition) {
				t.Fatalf("second transition error = %v, want ErrInvalidTransition", err)
			}
		})
	}
}

func TestPendingReferenceCanRejectOrFail(t *testing.T) {
	now := time.Now().UTC()
	for _, transition := range []struct {
		name string
		run  func(*WagerTransaction) error
		want Status
	}{
		{
			name: "reject",
			run: func(w *WagerTransaction) error {
				return w.Reject(FailureReferenceNotFound, now.Add(2*time.Second))
			},
			want: Rejected,
		},
		{
			name: "fail",
			run: func(w *WagerTransaction) error {
				return w.Fail(FailurePermanentInfrastructure, now.Add(2*time.Second))
			},
			want: Failed,
		},
	} {
		t.Run(transition.name, func(t *testing.T) {
			w := newWager(t, Refund, "10.00", "bet-1", now)
			if err := w.WaitForReference(now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if err := transition.run(w); err != nil {
				t.Fatalf("transition error = %v", err)
			}
			if w.Status() != transition.want {
				t.Fatalf("status = %s, want %s", w.Status(), transition.want)
			}
		})
	}
}

func TestInvalidTransitionsAndFailureCodes(t *testing.T) {
	now := time.Now().UTC()
	w := newWager(t, Bet, "1.00", "", now)
	if err := w.WaitForReference(now.Add(time.Second)); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("WaitForReference(BET) error = %v, want ErrInvalidTransition", err)
	}
	if err := w.Reject("", now.Add(time.Second)); !errors.Is(err, domain.ErrInvalidFailureCode) {
		t.Fatalf("Reject(empty code) error = %v, want ErrInvalidFailureCode", err)
	}
	if err := w.Fail(FailureCode("lowercase"), now.Add(time.Second)); !errors.Is(err, domain.ErrInvalidFailureCode) {
		t.Fatalf("Fail(invalid code) error = %v, want ErrInvalidFailureCode", err)
	}
	if err := w.Reject(FailureInvalidOperation, now.Add(-time.Second)); !errors.Is(err, domain.ErrInvalidTimestamp) {
		t.Fatalf("Reject(backwards time) error = %v, want ErrInvalidTimestamp", err)
	}
}

func TestProcessValidatesResultAndReference(t *testing.T) {
	now := time.Now().UTC()
	result := mustWagerMoney(t, "1.00", money.BRL)
	before := mustWagerMoney(t, "11.00", money.BRL)
	invalid := money.Money{}
	refund := newWager(t, Refund, "10.00", "bet-1", now)

	if err := refund.Process(before, result, nil, now.Add(time.Second)); !errors.Is(err, domain.ErrReferencePending) {
		t.Fatalf("Process without reference error = %v, want ErrReferencePending", err)
	}
	if err := refund.Process(before, invalid, nil, now.Add(time.Second)); !errors.Is(err, domain.ErrInvalidWager) {
		t.Fatalf("Process with invalid result error = %v, want ErrInvalidWager", err)
	}
	if err := refund.Process(before, mustWagerMoney(t, "-1.00", money.BRL), nil, now.Add(time.Second)); !errors.Is(err, domain.ErrInvalidWager) {
		t.Fatalf("Process with negative result error = %v, want ErrInvalidWager", err)
	}
	if err := refund.Process(
		mustWagerMoney(t, "100.00", money.BRL),
		mustWagerMoney(t, "90.00", money.BRL),
		processedReference(t, "bet-1", Bet, "10.00", now),
		now.Add(time.Second),
	); !errors.Is(err, domain.ErrInvalidWager) {
		t.Fatalf("Process with inconsistent balance error = %v, want ErrInvalidWager", err)
	}
}

func TestProcessEnforcesFinancialMovement(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name        string
		transaction func() (*WagerTransaction, *WagerTransaction)
		before      string
		after       string
		wantErr     error
	}{
		{
			name: "bet debits amount",
			transaction: func() (*WagerTransaction, *WagerTransaction) {
				return newWager(t, Bet, "10.00", "", now), nil
			},
			before: "100.00", after: "90.00",
		},
		{
			name: "win credits amount",
			transaction: func() (*WagerTransaction, *WagerTransaction) {
				return newWager(t, Win, "10.00", "", now), nil
			},
			before: "90.00", after: "100.00",
		},
		{
			name: "loss leaves balance unchanged",
			transaction: func() (*WagerTransaction, *WagerTransaction) {
				return newWager(t, Loss, "0.00", "", now), nil
			},
			before: "100.00", after: "100.00",
		},
		{
			name: "refund credits amount",
			transaction: func() (*WagerTransaction, *WagerTransaction) {
				refund := newWager(t, Refund, "10.00", "bet-1", now)
				return refund, processedReference(t, "bet-1", Bet, "10.00", now)
			},
			before: "90.00", after: "100.00",
		},
		{
			name: "rollback bet credits amount",
			transaction: func() (*WagerTransaction, *WagerTransaction) {
				rollback := newWager(t, Rollback, "10.00", "bet-1", now)
				return rollback, processedReference(t, "bet-1", Bet, "10.00", now)
			},
			before: "90.00", after: "100.00",
		},
		{
			name: "rollback win debits amount",
			transaction: func() (*WagerTransaction, *WagerTransaction) {
				rollback := newWager(t, Rollback, "10.00", "win-1", now)
				return rollback, processedReference(t, "win-1", Win, "10.00", now)
			},
			before: "100.00", after: "90.00",
		},
		{
			name: "reject inconsistent resulting balance",
			transaction: func() (*WagerTransaction, *WagerTransaction) {
				return newWager(t, Bet, "10.00", "", now), nil
			},
			before: "100.00", after: "91.00", wantErr: domain.ErrInvalidWager,
		},
		{
			name: "reject bet that would overdraw",
			transaction: func() (*WagerTransaction, *WagerTransaction) {
				return newWager(t, Bet, "10.00", "", now), nil
			},
			before: "5.00", after: "0.00", wantErr: domain.ErrInsufficientFunds,
		},
		{
			name: "reject rollback that would overdraw",
			transaction: func() (*WagerTransaction, *WagerTransaction) {
				rollback := newWager(t, Rollback, "10.00", "win-1", now)
				return rollback, processedReference(t, "win-1", Win, "10.00", now)
			},
			before: "5.00", after: "0.00", wantErr: domain.ErrReversalInsufficientFunds,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transaction, reference := test.transaction()
			err := transaction.Process(
				mustWagerMoney(t, test.before, money.BRL),
				mustWagerMoney(t, test.after, money.BRL),
				reference,
				now.Add(time.Second),
			)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("Process() error = %v, want %v", err, test.wantErr)
				}
				if transaction.Status() != Pending {
					t.Fatalf("failed Process() changed status to %s", transaction.Status())
				}
				return
			}
			if err != nil {
				t.Fatalf("Process() error = %v", err)
			}
			if transaction.Status() != Processed {
				t.Fatalf("status = %s, want PROCESSED", transaction.Status())
			}
		})
	}
}

func TestValidateReferenceRules(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name      string
		kind      Kind
		reference *WagerTransaction
		want      error
	}{
		{name: "missing refund reference", kind: Refund, want: domain.ErrReferencePending},
		{name: "pending reference", kind: Refund, reference: pendingReference(t, "bet-1", now), want: domain.ErrReferencePending},
		{name: "rejected reference", kind: Refund, reference: rejectedReference(t, "bet-1", now), want: domain.ErrReferenceNotEligible},
		{name: "wrong external id", kind: Refund, reference: processedReference(t, "other", Bet, "10.00", now), want: domain.ErrReferenceMismatch},
		{name: "wrong kind for refund", kind: Refund, reference: processedReference(t, "bet-1", Win, "10.00", now), want: domain.ErrReferenceNotEligible},
		{name: "wrong kind for rollback", kind: Rollback, reference: processedReference(t, "bet-1", Loss, "0.00", now), want: domain.ErrReferenceNotEligible},
		{name: "amount mismatch", kind: Refund, reference: processedReference(t, "bet-1", Bet, "11.00", now), want: domain.ErrReferenceAmountMismatch},
		{name: "currency mismatch", kind: Refund, reference: processedReferenceCurrency(t, "bet-1", Bet, "10.00", money.USD, now), want: domain.ErrCurrencyMismatch},
		{name: "provider mismatch", kind: Refund, reference: processedReferenceWith(t, func(p *ExternalParams) { p.ProviderID = "provider-b" }, "bet-1", Bet, "10.00", now), want: domain.ErrReferenceMismatch},
		{name: "player mismatch", kind: Refund, reference: processedReferenceWith(t, func(p *ExternalParams) { p.PlayerID = "0192f299-27dd-7d3f-8071-5f8685deef37" }, "bet-1", Bet, "10.00", now), want: domain.ErrReferenceMismatch},
		{name: "wallet mismatch", kind: Refund, reference: processedReferenceWith(t, func(p *ExternalParams) { p.WalletID = "0192f298-27dd-7d3f-8071-5f8685deef37" }, "bet-1", Bet, "10.00", now), want: domain.ErrReferenceMismatch},
		{name: "round mismatch", kind: Refund, reference: processedReferenceWith(t, func(p *ExternalParams) { p.RoundID = "round-other" }, "bet-1", Bet, "10.00", now), want: domain.ErrReferenceMismatch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transaction := newWager(t, test.kind, "10.00", "bet-1", now)
			err := transaction.ValidateReference(test.reference)
			if !errors.Is(err, test.want) {
				t.Fatalf("ValidateReference() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestMovementForEveryFinancialKind(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name      string
		kind      Kind
		amount    string
		reference Kind
		wantDir   Direction
		wantMove  bool
		wantErr   error
	}{
		{name: "BET debit", kind: Bet, amount: "10.00", wantDir: Debit, wantMove: true},
		{name: "WIN credit", kind: Win, amount: "10.00", wantDir: Credit, wantMove: true},
		{name: "LOSS no movement", kind: Loss, amount: "0.00"},
		{name: "REFUND credit", kind: Refund, amount: "10.00", reference: Bet, wantDir: Credit, wantMove: true},
		{name: "ROLLBACK BET credit", kind: Rollback, amount: "10.00", reference: Bet, wantDir: Credit, wantMove: true},
		{name: "ROLLBACK WIN debit", kind: Rollback, amount: "10.00", reference: Win, wantDir: Debit, wantMove: true},
		{name: "ROLLBACK REFUND debit", kind: Rollback, amount: "10.00", reference: Refund, wantDir: Debit, wantMove: true},
		{name: "OPENING credit", kind: Opening, amount: "10.00", wantDir: Credit, wantMove: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transaction := newMovementWager(t, test.kind, test.amount, now)
			var reference *WagerTransaction
			if test.reference != "" {
				externalID := transaction.ReferenceExternalTransactionID()
				if test.reference == Win {
					externalID = transaction.ReferenceExternalTransactionID()
				}
				if test.reference == Refund {
					externalID = transaction.ReferenceExternalTransactionID()
				}
				if test.reference == Refund {
					reference = processedRefundReference(t, externalID, "10.00", now)
				} else {
					reference = processedReference(t, externalID, test.reference, test.amount, now)
				}
			}
			direction, moves, err := transaction.Movement(reference)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("Movement() error = %v, want %v", err, test.wantErr)
				}
				return
			}
			if err != nil || direction != test.wantDir || moves != test.wantMove {
				t.Fatalf("Movement() = %s, %t, %v; want %s, %t, nil", direction, moves, err, test.wantDir, test.wantMove)
			}
		})
	}
}

func TestRehydrateValidatesPersistedState(t *testing.T) {
	now := time.Now().UTC()
	base := newWager(t, Bet, "10.00", "", now).Snapshot()
	processed := base
	processed.Status = Processed
	processed.ResultBalance = mustWagerMoney(t, "90.00", money.BRL)
	processed.HasResultBalance = true
	processed.UpdatedAt = now.Add(time.Second)
	rejected := base
	rejected.Status = Rejected
	rejected.FailureCode = FailureInsufficientFunds
	rejected.UpdatedAt = now.Add(time.Second)
	pendingReference := newWager(t, Refund, "10.00", "bet-1", now).Snapshot()
	pendingReference.Status = PendingReference
	pendingReference.UpdatedAt = now.Add(time.Second)

	tests := []struct {
		name     string
		snapshot Snapshot
		wantErr  bool
	}{
		{name: "pending", snapshot: base},
		{name: "processed", snapshot: processed},
		{name: "rejected", snapshot: rejected},
		{name: "pending reference", snapshot: pendingReference},
		{name: "invalid status", snapshot: func() Snapshot { s := base; s.Status = "UNKNOWN"; return s }(), wantErr: true},
		{name: "processed without result", snapshot: func() Snapshot { s := processed; s.HasResultBalance = false; return s }(), wantErr: true},
		{name: "rejected without failure", snapshot: func() Snapshot { s := rejected; s.FailureCode = ""; return s }(), wantErr: true},
		{name: "pending reference for bet", snapshot: func() Snapshot { s := base; s.Status = PendingReference; return s }(), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			restored, err := Rehydrate(test.snapshot)
			if (err != nil) != test.wantErr {
				t.Fatalf("Rehydrate() error = %v, wantErr %t", err, test.wantErr)
			}
			if err == nil && restored.Status() != test.snapshot.Status {
				t.Fatalf("restored status = %s, want %s", restored.Status(), test.snapshot.Status)
			}
		})
	}
}

func TestSnapshotReturnsIndependentValue(t *testing.T) {
	transaction := newWager(t, Bet, "1.00", "", time.Now())
	snapshot := transaction.Snapshot()
	snapshot.Status = Processed
	if transaction.Status() != Pending {
		t.Fatalf("mutating snapshot changed transaction status to %s", transaction.Status())
	}
}

func validExternalParams(t *testing.T, kind Kind, amount, reference string) ExternalParams {
	t.Helper()
	return ExternalParams{
		ID:                             wagerID,
		ProviderID:                     "provider-a",
		ExternalTransactionID:          "txn-123",
		IdempotencyKey:                 "provider-a:txn-123",
		PayloadHash:                    strings.Repeat("a", 64),
		WalletID:                       wagerWalletID,
		PlayerID:                       wagerPlayerID,
		RoundID:                        "round-1",
		GameID:                         "game-1",
		Kind:                           kind,
		Amount:                         mustWagerMoney(t, amount, money.BRL),
		ReferenceExternalTransactionID: reference,
		CreatedAt:                      time.Now().UTC(),
	}
}

func newWager(t *testing.T, kind Kind, amount, reference string, created time.Time) *WagerTransaction {
	t.Helper()
	params := validExternalParams(t, kind, amount, reference)
	params.CreatedAt = created
	transaction, err := NewExternal(params)
	if err != nil {
		t.Fatalf("NewExternal() error = %v", err)
	}
	return transaction
}

func processedReference(t *testing.T, externalID string, kind Kind, amount string, created time.Time) *WagerTransaction {
	t.Helper()
	params := validExternalParams(t, kind, amount, "")
	if kind == Refund || kind == Rollback {
		params.ReferenceExternalTransactionID = "base-bet"
	}
	params.ExternalTransactionID = externalID
	params.IdempotencyKey = "provider-a:" + externalID
	params.CreatedAt = created
	transaction, err := NewExternal(params)
	if err != nil {
		t.Fatalf("NewExternal(reference) error = %v", err)
	}
	result := mustWagerMoney(t, "100.00", money.BRL)
	if kind == Loss {
		result = mustWagerMoney(t, "100.00", money.BRL)
	}
	before := processedBalanceBefore(t, kind, params.Amount, result)
	if err := transaction.Process(before, result, nil, created.Add(time.Second)); err != nil {
		t.Fatalf("Process(reference) error = %v", err)
	}
	return transaction
}

func processedRefundReference(t *testing.T, externalID, amount string, created time.Time) *WagerTransaction {
	t.Helper()
	params := validExternalParams(t, Refund, amount, "base-bet")
	params.ExternalTransactionID = externalID
	params.IdempotencyKey = "provider-a:" + externalID
	params.CreatedAt = created
	transaction, err := NewExternal(params)
	if err != nil {
		t.Fatalf("NewExternal(refund reference) error = %v", err)
	}
	bet := processedReference(t, "base-bet", Bet, amount, created)
	if err := transaction.Process(
		mustWagerMoney(t, "90.00", money.BRL),
		mustWagerMoney(t, "100.00", money.BRL),
		bet,
		created.Add(time.Second),
	); err != nil {
		t.Fatalf("Process(refund reference) error = %v", err)
	}
	return transaction
}

func processedReferenceCurrency(t *testing.T, externalID string, kind Kind, amount string, currency money.Currency, created time.Time) *WagerTransaction {
	t.Helper()
	params := validExternalParams(t, kind, amount, "")
	params.ExternalTransactionID = externalID
	params.IdempotencyKey = "provider-a:" + externalID
	params.Amount = mustWagerMoney(t, amount, currency)
	params.CreatedAt = created
	transaction, err := NewExternal(params)
	if err != nil {
		t.Fatalf("NewExternal(reference) error = %v", err)
	}
	result := mustWagerMoney(t, "100.00", currency)
	before := processedBalanceBefore(t, kind, params.Amount, result)
	if err := transaction.Process(before, result, nil, created.Add(time.Second)); err != nil {
		t.Fatalf("Process(reference) error = %v", err)
	}
	return transaction
}

func processedReferenceWith(
	t *testing.T,
	change func(*ExternalParams),
	externalID string,
	kind Kind,
	amount string,
	created time.Time,
) *WagerTransaction {
	t.Helper()
	params := validExternalParams(t, kind, amount, "")
	params.ExternalTransactionID = externalID
	params.IdempotencyKey = "provider-a:" + externalID
	params.CreatedAt = created
	change(&params)
	transaction, err := NewExternal(params)
	if err != nil {
		t.Fatalf("NewExternal(reference) error = %v", err)
	}
	after := mustWagerMoney(t, "100.00", money.BRL)
	before := processedBalanceBefore(t, transaction.Kind(), params.Amount, after)
	if err := transaction.Process(before, after, nil, created.Add(time.Second)); err != nil {
		t.Fatalf("Process(reference) error = %v", err)
	}
	return transaction
}

func processedBalanceBefore(t *testing.T, kind Kind, amount, after money.Money) money.Money {
	t.Helper()
	switch kind {
	case Bet:
		before, err := after.Add(amount)
		if err != nil {
			t.Fatalf("calculate processed BET balance: %v", err)
		}
		return before
	case Win:
		before, err := after.Sub(amount)
		if err != nil {
			t.Fatalf("calculate processed WIN balance: %v", err)
		}
		return before
	default:
		return after
	}
}

func rejectedReference(t *testing.T, externalID string, created time.Time) *WagerTransaction {
	t.Helper()
	transaction := newWager(t, Bet, "10.00", "", created)
	transaction.snapshot.ExternalTransactionID = externalID
	transaction.snapshot.IdempotencyKey = "provider-a:" + externalID
	if err := transaction.Reject(FailureInsufficientFunds, created.Add(time.Second)); err != nil {
		t.Fatalf("Reject(reference) error = %v", err)
	}
	return transaction
}

func pendingReference(t *testing.T, externalID string, created time.Time) *WagerTransaction {
	t.Helper()
	params := validExternalParams(t, Bet, "10.00", "")
	params.ExternalTransactionID = externalID
	params.IdempotencyKey = "provider-a:" + externalID
	params.CreatedAt = created
	transaction, err := NewExternal(params)
	if err != nil {
		t.Fatalf("NewExternal(reference) error = %v", err)
	}
	return transaction
}

func newMovementWager(t *testing.T, kind Kind, amount string, now time.Time) *WagerTransaction {
	t.Helper()
	if kind == Opening {
		transaction, err := NewOpening(OpeningParams{
			ID: wagerID, WalletID: wagerWalletID, PlayerID: wagerPlayerID,
			Amount: mustWagerMoney(t, amount, money.BRL), CreatedAt: now,
		})
		if err != nil {
			t.Fatalf("NewOpening() error = %v", err)
		}
		return transaction
	}
	referenceID := ""
	if kind == Refund || kind == Rollback {
		referenceID = "reference-1"
	}
	return newWager(t, kind, amount, referenceID, now)
}

func mustWagerMoney(t *testing.T, amount string, currency money.Currency) money.Money {
	t.Helper()
	result, err := money.ParseAmount(amount, currency)
	if err != nil {
		t.Fatalf("ParseAmount(%q): %v", amount, err)
	}
	return result
}
