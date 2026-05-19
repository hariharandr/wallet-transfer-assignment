package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/hariharandr/wallet-transfer-assignment/internal/apperr"
	"github.com/hariharandr/wallet-transfer-assignment/internal/domain"
)

// what we report back to a successful caller, also stored for replay.
const replayHTTPStatus = 201

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

// what we keep in the idempotency row so a duplicate gets the same answer.
type storedResult struct {
	TransferID string `json:"transferId"`
	State      string `json:"state"`
	From       string `json:"from"`
	To         string `json:"to"`
	Amount     int64  `json:"amount"`
}

func (r Request) validate() error {
	if r.IdempotencyKey == "" {
		return fmt.Errorf("%w: idempotency key is required", apperr.ErrInvalidRequest)
	}
	if r.FromWalletID == "" || r.ToWalletID == "" {
		return fmt.Errorf("%w: wallet ids are required", apperr.ErrInvalidRequest)
	}
	if r.FromWalletID == r.ToWalletID {
		return apperr.ErrSameWallet
	}
	return domain.ValidateAmount(r.Amount)
}

// Fingerprint is a stable hash of the money fields. lets us catch the
// same key being sent with a different payload.
func Fingerprint(from, to string, amount int64) string {
	h := sha256.Sum256([]byte(from + "|" + to + "|" + strconv.FormatInt(amount, 10)))
	return hex.EncodeToString(h[:])
}

func (s *Service) Transfer(ctx context.Context, req Request) (Result, error) {
	if err := req.validate(); err != nil {
		return Result{}, err
	}

	fp := Fingerprint(req.FromWalletID, req.ToWalletID, req.Amount)

	// claim the key first. winner of this insert does the work, anyone
	// else is a duplicate and goes down the replay path.
	inserted, err := s.repo.InsertIdempotencyPending(ctx, req.IdempotencyKey, fp)
	if err != nil {
		return Result{}, err
	}
	if !inserted {
		return s.replay(ctx, req, fp)
	}

	var res Result
	err = s.repo.WithinTx(ctx, func(tx Tx) error {
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

		for _, le := range []domain.LedgerEntry{
			{TransferID: id, WalletID: req.FromWalletID, Type: domain.Debit, Amount: req.Amount},
			{TransferID: id, WalletID: req.ToWalletID, Type: domain.Credit, Amount: req.Amount},
		} {
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
			TransferID: id, State: domain.StateProcessed,
			From: req.FromWalletID, To: req.ToWalletID, Amount: req.Amount,
		}

		body, e := json.Marshal(storedResult{
			TransferID: id, State: domain.StateProcessed.String(),
			From: req.FromWalletID, To: req.ToWalletID, Amount: req.Amount,
		})
		if e != nil {
			return e
		}
		return tx.CompleteIdempotency(ctx, req.IdempotencyKey, id, replayHTTPStatus, body)
	})
	if err != nil {
		return Result{}, err
	}
	return res, nil
}

// replay deals with a key we already saw.
func (s *Service) replay(ctx context.Context, req Request, fp string) (Result, error) {
	rec, err := s.repo.LoadIdempotency(ctx, req.IdempotencyKey)
	if err != nil {
		return Result{}, err
	}
	if rec.Fingerprint != fp {
		return Result{}, apperr.ErrKeyReused
	}
	if rec.Status != "COMPLETED" {
		// the original request is still running somewhere
		return Result{}, apperr.ErrInProgress
	}

	var sr storedResult
	if err := json.Unmarshal(rec.ResponseBody, &sr); err != nil {
		return Result{}, fmt.Errorf("decode stored result: %w", err)
	}
	var st domain.TransferState
	if err := st.Scan(sr.State); err != nil {
		return Result{}, err
	}
	return Result{
		TransferID: sr.TransferID, State: st,
		From: sr.From, To: sr.To, Amount: sr.Amount,
	}, nil
}
