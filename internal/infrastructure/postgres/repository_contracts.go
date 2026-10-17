package postgres

import "github.com/kassiobuck/backend-challenge-go/internal/application/port"

var (
	_ port.WalletRepository      = (*WalletRepository)(nil)
	_ port.WagerRepository       = (*WagerRepository)(nil)
	_ port.LedgerRepository      = (*LedgerRepository)(nil)
	_ port.IdempotencyRepository = (*IdempotencyRepository)(nil)
	_ port.InboxRepository       = (*InboxRepository)(nil)
	_ port.OutboxRepository      = (*OutboxRepository)(nil)
	_ port.ReversalRepository    = (*ReversalRepository)(nil)
)
