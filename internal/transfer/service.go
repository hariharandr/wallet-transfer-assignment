package transfer

import (
	"context"
	"fmt"

	"github.com/hariharandr/wallet-transfer-assignment/internal/apperr"
	"github.com/hariharandr/wallet-transfer-assignment/internal/domain"
)

type Service struct {
	repo Repository
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

type Request struct {
	IdempotencyKey string
	FromWalletID   string
	ToWalletID     string
	Amount         int64
}

type Result struct {
	TransferID string
	State      domain.TransferState
	From       string
	To         string
	Amount     int64
}

func (r Request) validate() error {
	if r.FromWalletID == "" || r.ToWalletID == "" {
		return fmt.Errorf("%w: wallet ids are required", apperr.ErrInvalidRequest)
	}
	if r.FromWalletID == r.ToWalletID {
		return apperr.ErrSameWallet
	}
	return domain.ValidateAmount(r.Amount)
}

// Transfer does one wallet to wallet move inside a single db tx.
func (s *Service) Transfer(ctx context.Context, req Request) (Result, error) {
	if err := req.validate(); err != nil {
		return Result{}, err
	}

	var res Result
	err := s.repo.WithinTx(ctx, func(tx Tx) error {
		// lock both rows up front, this is what stops concurrent
		// debits from racing on the same wallet.
		if _, e := tx.LockWallets(ctx, req.FromWalletID, req.ToWalletID); e != nil {
			return e
		}

		id, e := tx.InsertTransfer(ctx, domain.Transfer{
			FromWallet: req.FromWalletID,
			ToWallet:   req.ToWalletID,
			Amount:     req.Amount,
			State:      domain.StatePending,
		})
		if e != nil {
			return e
		}

		// every transfer is exactly one debit and one credit.
		entries := []domain.LedgerEntry{
			{TransferID: id, WalletID: req.FromWalletID, Type: domain.Debit, Amount: req.Amount},
			{TransferID: id, WalletID: req.ToWalletID, Type: domain.Credit, Amount: req.Amount},
		}
		for _, le := range entries {
			if e = tx.InsertLedgerEntry(ctx, le); e != nil {
				return e
			}
		}

		if e = tx.AdjustBalance(ctx, req.FromWalletID, -req.Amount); e != nil {
			return e
		}
		if e = tx.AdjustBalance(ctx, req.ToWalletID, req.Amount); e != nil {
			return e
		}

		if e = tx.UpdateTransferState(ctx, id, domain.StateProcessed, ""); e != nil {
			return e
		}

		res = Result{
			TransferID: id,
			State:      domain.StateProcessed,
			From:       req.FromWalletID,
			To:         req.ToWalletID,
			Amount:     req.Amount,
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return res, nil
}
