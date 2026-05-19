// apperr holds the typed errors that cross layer boundaries.
package apperr

import "errors"

var (
	ErrWalletNotFound    = errors.New("wallet not found")
	ErrInsufficientFunds = errors.New("insufficient funds")
	ErrKeyReused         = errors.New("idempotency key reused with different parameters")
	ErrInProgress        = errors.New("request in progress")
)
