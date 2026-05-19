package transfer

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hariharandr/wallet-transfer-assignment/internal/apperr"
	"github.com/hariharandr/wallet-transfer-assignment/internal/domain"
)

// Tx is the db work the service does inside a single transaction.
type Tx interface {
	LockWallets(ctx context.Context, ids ...string) (map[string]domain.Wallet, error)
	InsertTransfer(ctx context.Context, t domain.Transfer) (string, error)
	UpdateTransferState(ctx context.Context, id string, st domain.TransferState, reason string) error
	InsertLedgerEntry(ctx context.Context, e domain.LedgerEntry) error
	AdjustBalance(ctx context.Context, walletID string, delta int64) error
}

// Repository owns the transaction boundary.
type Repository interface {
	WithinTx(ctx context.Context, fn func(tx Tx) error) error
}

type PgxRepository struct {
	pool *pgxpool.Pool
}

func NewPgxRepository(pool *pgxpool.Pool) *PgxRepository {
	return &PgxRepository{pool: pool}
}

// WithinTx runs fn in a tx. commit on nil error, otherwise rollback.
func (r *PgxRepository) WithinTx(ctx context.Context, fn func(tx Tx) error) error {
	pgtx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	// safe even after commit, pgx just returns tx closed which we ignore.
	defer func() { _ = pgtx.Rollback(ctx) }()

	if err := fn(&pgxTx{tx: pgtx}); err != nil {
		return err
	}
	if err := pgtx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

type pgxTx struct {
	tx pgx.Tx
}

// LockWallets locks the rows in id order so two transfers on the same
// pair cant deadlock each other.
func (p *pgxTx) LockWallets(ctx context.Context, ids ...string) (map[string]domain.Wallet, error) {
	rows, err := p.tx.Query(ctx,
		`select id, balance from wallets where id = any($1) order by id for update`, ids)
	if err != nil {
		return nil, fmt.Errorf("lock wallets: %w", err)
	}
	defer rows.Close()

	out := make(map[string]domain.Wallet, len(ids))
	for rows.Next() {
		var w domain.Wallet
		if err := rows.Scan(&w.ID, &w.Balance); err != nil {
			return nil, err
		}
		out[w.ID] = w
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, ok := out[id]; !ok {
			return nil, fmt.Errorf("%w: %s", apperr.ErrWalletNotFound, id)
		}
	}
	return out, nil
}

func (p *pgxTx) InsertTransfer(ctx context.Context, t domain.Transfer) (string, error) {
	var id string
	err := p.tx.QueryRow(ctx,
		`insert into transfers (from_wallet, to_wallet, amount, status)
		 values ($1,$2,$3,$4) returning id`,
		t.FromWallet, t.ToWallet, t.Amount, t.State).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert transfer: %w", err)
	}
	return id, nil
}

func (p *pgxTx) UpdateTransferState(ctx context.Context, id string, st domain.TransferState, reason string) error {
	var fr any
	if reason != "" {
		fr = reason
	}
	ct, err := p.tx.Exec(ctx,
		`update transfers set status=$1, failure_reason=$2 where id=$3`, st, fr, id)
	if err != nil {
		return fmt.Errorf("update transfer state: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("transfer %s not found for state update", id)
	}
	return nil
}

func (p *pgxTx) InsertLedgerEntry(ctx context.Context, e domain.LedgerEntry) error {
	_, err := p.tx.Exec(ctx,
		`insert into ledger_entries (transfer_id, wallet_id, type, amount)
		 values ($1,$2,$3,$4)`, e.TransferID, e.WalletID, e.Type, e.Amount)
	if err != nil {
		return fmt.Errorf("insert ledger entry: %w", err)
	}
	return nil
}

// AdjustBalance moves balance by delta. the db check (balance >= 0) is
// the last guard against overdraft if app logic ever slips.
func (p *pgxTx) AdjustBalance(ctx context.Context, walletID string, delta int64) error {
	_, err := p.tx.Exec(ctx,
		`update wallets set balance = balance + $1, updated_at = now() where id = $2`,
		delta, walletID)
	if err != nil {
		return fmt.Errorf("adjust balance: %w", err)
	}
	return nil
}

var (
	_ Repository = (*PgxRepository)(nil)
	_ Tx         = (*pgxTx)(nil)
)
