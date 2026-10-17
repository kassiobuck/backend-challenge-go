package domain

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidID                 = errors.New("invalid domain identifier")
	ErrInvalidTimestamp          = errors.New("invalid timestamp")
	ErrInvalidMoney              = errors.New("invalid money")
	ErrInvalidCurrency           = errors.New("invalid currency")
	ErrCurrencyMismatch          = errors.New("currency mismatch")
	ErrUnsupportedCurrency       = errors.New("unsupported currency")
	ErrOverflow                  = errors.New("money overflow")
	ErrInvalidWallet             = errors.New("invalid wallet")
	ErrInvalidWalletState        = errors.New("invalid wallet state")
	ErrInsufficientFunds         = errors.New("insufficient funds")
	ErrReversalInsufficientFunds = fmt.Errorf("%w: reversal", ErrInsufficientFunds)
	ErrVersionOverflow           = errors.New("wallet version overflow")
	ErrInvalidWager              = errors.New("invalid wager transaction")
	ErrInvalidWagerKind          = errors.New("invalid wager kind")
	ErrInvalidWagerState         = errors.New("invalid wager state")
	ErrInvalidTransition         = errors.New("invalid wager state transition")
	ErrInvalidFailureCode        = errors.New("invalid failure code")
	ErrReferenceRequired         = errors.New("reference is required")
	ErrReferencePending          = errors.New("reference is not available yet")
	ErrReferenceNotEligible      = errors.New("reference is not eligible")
	ErrReferenceMismatch         = errors.New("reference does not match wager")
	ErrReferenceAmountMismatch   = errors.New("reference amount does not match wager")
	ErrInvalidLedgerEntry        = errors.New("invalid wallet ledger entry")
	ErrInvalidEvent              = errors.New("invalid domain event")
)
