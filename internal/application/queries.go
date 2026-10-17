package application

import (
	"context"
	"errors"
	"fmt"

	"github.com/kassiobuck/backend-challenge-go/internal/application/port"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/ledger"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/money"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/wager"
	"github.com/kassiobuck/backend-challenge-go/internal/domain/wallet"
)

type LedgerPage struct {
	Entries []ledger.WalletLedgerEntry
	HasMore bool
}

type Reconciliation struct {
	Wallet        *wallet.Wallet
	LedgerBalance money.Money
	EntryCount    int64
	Reconciled    bool
}

func (s *FinancialService) GetWallet(ctx context.Context, walletID string) (*wallet.Wallet, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	var result *wallet.Wallet
	err := s.unitOfWork.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
		var err error
		result, err = repos.Wallets().FindByID(ctx, walletID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("get wallet: %w", err)
	}
	return result, nil
}

func (s *FinancialService) ListWalletLedger(
	ctx context.Context,
	walletID string,
	cursor *port.LedgerCursor,
	limit int,
) (LedgerPage, error) {
	if err := s.validate(); err != nil {
		return LedgerPage{}, err
	}
	if limit <= 0 || limit >= 1000 {
		return LedgerPage{}, errors.New("ledger limit must be between 1 and 999")
	}
	var entries []ledger.WalletLedgerEntry
	err := s.unitOfWork.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
		if _, err := repos.Wallets().FindByID(ctx, walletID); err != nil {
			return err
		}
		var err error
		entries, err = repos.Ledger().List(ctx, walletID, cursor, limit+1)
		return err
	})
	if err != nil {
		return LedgerPage{}, fmt.Errorf("list wallet ledger: %w", err)
	}
	page := LedgerPage{Entries: entries}
	if len(entries) > limit {
		page.HasMore = true
		page.Entries = entries[:limit]
	}
	return page, nil
}

func (s *FinancialService) GetWager(ctx context.Context, transactionID string) (*wager.WagerTransaction, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	var result *wager.WagerTransaction
	err := s.unitOfWork.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
		var err error
		result, err = repos.Wagers().FindByID(ctx, transactionID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("get wager transaction: %w", err)
	}
	return result, nil
}

func (s *FinancialService) GetProviderWager(
	ctx context.Context,
	providerID, externalTransactionID string,
) (*wager.WagerTransaction, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	var result *wager.WagerTransaction
	err := s.unitOfWork.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
		var err error
		result, err = repos.Wagers().FindByExternalID(ctx, providerID, externalTransactionID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("get provider wager transaction: %w", err)
	}
	return result, nil
}

func (s *FinancialService) ReconcileWallet(ctx context.Context, walletID string) (*Reconciliation, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	var result *Reconciliation
	err := s.unitOfWork.Within(ctx, func(ctx context.Context, repos port.Repositories) error {
		entity, err := repos.Wallets().LockByID(ctx, walletID)
		if err != nil {
			return err
		}
		totals, err := repos.Ledger().Reconcile(ctx, walletID)
		if err != nil {
			return err
		}
		credits, err := money.NewMinor(totals.CreditsMinor, entity.Currency())
		if err != nil {
			return err
		}
		debits, err := money.NewMinor(totals.DebitsMinor, entity.Currency())
		if err != nil {
			return err
		}
		balance, err := credits.Sub(debits)
		if err != nil {
			return err
		}
		result = &Reconciliation{
			Wallet: entity, LedgerBalance: balance, EntryCount: totals.EntryCount,
			Reconciled: balance == entity.Balance(),
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reconcile wallet: %w", err)
	}
	return result, nil
}
