// Package deposits is the durable record of every deposit this watcher
// detects -- finality.DepositStore, backed by the deposits table. A
// deposit is recorded before the scan cursor moves past it, so a restart
// can never lose one that was seen but not yet reported to the ledger.
package deposits

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"

	"depositwatcher/internal/chain"
	"depositwatcher/internal/db"
	"depositwatcher/internal/finality"
	"depositwatcher/internal/money"
)

// Store implements finality.DepositStore.
type Store struct {
	q db.Queryer
}

// NewStore wires a Store.
func NewStore(q db.Queryer) *Store { return &Store{q: q} }

var _ finality.DepositStore = (*Store)(nil)

// Save records c as detected, or reports that it was already recorded.
func (s *Store) Save(ctx context.Context, c finality.Candidate) (bool, string, error) {
	tag, err := s.q.Exec(ctx, `
		INSERT INTO deposits
			(tx_hash, log_index, address, order_id, external_id, customer_id, amount, sender_address,
			 height, block_time, classification)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (tx_hash, log_index) DO NOTHING
	`, c.TxHash.Hex(), int(c.LogIndex), c.Address, c.OrderID, c.ExternalID, c.CustomerID, int64(c.Amount),
		c.SenderAddress, int64(c.Height), c.BlockTime, int(c.Classification))
	if err != nil {
		return false, "", fmt.Errorf("deposits: saving %s:%d: %w", c.TxHash.Hex(), c.LogIndex, err)
	}
	if tag.RowsAffected() == 1 {
		return false, finality.DepositDetected, nil
	}
	var status string
	if err := s.q.QueryRow(ctx, `SELECT status FROM deposits WHERE tx_hash = $1 AND log_index = $2`,
		c.TxHash.Hex(), int(c.LogIndex)).Scan(&status); err != nil {
		return false, "", fmt.Errorf("deposits: reading %s:%d: %w", c.TxHash.Hex(), c.LogIndex, err)
	}
	return true, status, nil
}

// SetStatus records a deposit's outcome.
func (s *Store) SetStatus(ctx context.Context, txHash common.Hash, logIndex uint, status, note string) error {
	var n *string
	if note != "" {
		n = &note
	}
	if _, err := s.q.Exec(ctx, `
		UPDATE deposits SET status = $3, note = $4, updated_at = now() WHERE tx_hash = $1 AND log_index = $2
	`, txHash.Hex(), int(logIndex), status, n); err != nil {
		return fmt.Errorf("deposits: marking %s:%d %s: %w", txHash.Hex(), logIndex, status, err)
	}
	return nil
}

// LoadDetected returns every deposit still awaiting a report.
func (s *Store) LoadDetected(ctx context.Context) ([]finality.Candidate, error) {
	rows, err := s.q.Query(ctx, `
		SELECT tx_hash, log_index, address, order_id, external_id, customer_id, amount, sender_address,
			height, block_time, classification, detected_at
		FROM deposits WHERE status = 'DETECTED' ORDER BY height, tx_hash, log_index`)
	if err != nil {
		return nil, fmt.Errorf("deposits: loading detected deposits: %w", err)
	}
	defer rows.Close()
	var out []finality.Candidate
	for rows.Next() {
		var c finality.Candidate
		var txHash string
		var logIndex, classification int
		var amount, height int64
		if err := rows.Scan(&txHash, &logIndex, &c.Address, &c.OrderID, &c.ExternalID, &c.CustomerID, &amount,
			&c.SenderAddress, &height, &c.BlockTime, &classification, &c.DetectedAt); err != nil {
			return nil, fmt.Errorf("deposits: loading detected deposits: %w", err)
		}
		c.TxHash, c.LogIndex = common.HexToHash(txHash), uint(logIndex)
		c.Amount, c.Height = money.Amount(amount), uint64(height)
		c.Classification = chain.Classification(classification)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Deposit is one recorded deposit, for operators and relayd.
type Deposit struct {
	TxHash        string
	LogIndex      int
	Address       string
	OrderID       int64
	Amount        money.Amount
	SenderAddress string
	Height        int64
	Status        string
	Note          *string
}

// ForOrder returns every deposit recorded against orderID.
func ForOrder(ctx context.Context, q db.Queryer, orderID int64) ([]Deposit, error) {
	rows, err := q.Query(ctx, `
		SELECT tx_hash, log_index, address, order_id, amount, sender_address, height, status, note
		FROM deposits WHERE order_id = $1 ORDER BY height, log_index`, orderID)
	if err != nil {
		return nil, fmt.Errorf("deposits: listing deposits for order %d: %w", orderID, err)
	}
	defer rows.Close()
	var out []Deposit
	for rows.Next() {
		var d Deposit
		var amount int64
		if err := rows.Scan(&d.TxHash, &d.LogIndex, &d.Address, &d.OrderID, &amount, &d.SenderAddress, &d.Height, &d.Status, &d.Note); err != nil {
			return nil, fmt.Errorf("deposits: listing deposits for order %d: %w", orderID, err)
		}
		d.Amount = money.Amount(amount)
		out = append(out, d)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	return out, nil
}
