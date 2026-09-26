// Package deposits is the durable record of every deposit this watcher
// detects -- finality.DepositStore, backed by the deposits table. A
// deposit is recorded before its wallet's scan cursor moves past it, so a
// restart can never lose one that was seen but not yet reported.
package deposits

import (
	"context"
	"fmt"

	"tronwatcher/internal/chain"
	"tronwatcher/internal/db"
	"tronwatcher/internal/finality"
	"tronwatcher/internal/money"
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
			(tx_id, address, order_id, external_id, customer_id, amount, sender_address, block_time, classification)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (tx_id) DO NOTHING
	`, c.TxID, c.Address, c.OrderID, c.ExternalID, c.CustomerID, int64(c.Amount), c.SenderAddress, c.BlockTimestamp, int(c.Classification))
	if err != nil {
		return false, "", fmt.Errorf("deposits: saving %s: %w", c.TxID, err)
	}
	if tag.RowsAffected() == 1 {
		return false, finality.DepositDetected, nil
	}
	var status string
	if err := s.q.QueryRow(ctx, `SELECT status FROM deposits WHERE tx_id = $1`, c.TxID).Scan(&status); err != nil {
		return false, "", fmt.Errorf("deposits: reading %s: %w", c.TxID, err)
	}
	return true, status, nil
}

// SetStatus records a deposit's outcome.
func (s *Store) SetStatus(ctx context.Context, txID, status, note string) error {
	var n *string
	if note != "" {
		n = &note
	}
	if _, err := s.q.Exec(ctx, `UPDATE deposits SET status = $2, note = $3, updated_at = now() WHERE tx_id = $1`,
		txID, status, n); err != nil {
		return fmt.Errorf("deposits: marking %s %s: %w", txID, status, err)
	}
	return nil
}

// LoadDetected returns every deposit still awaiting a report.
func (s *Store) LoadDetected(ctx context.Context) ([]finality.Candidate, error) {
	rows, err := s.q.Query(ctx, `
		SELECT tx_id, address, order_id, external_id, customer_id, amount, sender_address, block_time, classification, detected_at
		FROM deposits WHERE status = 'DETECTED' ORDER BY block_time, tx_id`)
	if err != nil {
		return nil, fmt.Errorf("deposits: loading detected deposits: %w", err)
	}
	defer rows.Close()
	var out []finality.Candidate
	for rows.Next() {
		var c finality.Candidate
		var amount int64
		var classification int
		if err := rows.Scan(&c.TxID, &c.Address, &c.OrderID, &c.ExternalID, &c.CustomerID, &amount,
			&c.SenderAddress, &c.BlockTimestamp, &classification, &c.DetectedAt); err != nil {
			return nil, fmt.Errorf("deposits: loading detected deposits: %w", err)
		}
		c.Amount = money.Amount(amount)
		c.Classification = chain.Classification(classification)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Deposit is one recorded deposit, for operators and relayd.
type Deposit struct {
	TxID          string
	Address       string
	OrderID       int64
	Amount        money.Amount
	SenderAddress string
	Status        string
	Note          *string
}

// ForOrder returns every deposit recorded against orderID.
func ForOrder(ctx context.Context, q db.Queryer, orderID int64) ([]Deposit, error) {
	rows, err := q.Query(ctx, `
		SELECT tx_id, address, order_id, amount, sender_address, status, note
		FROM deposits WHERE order_id = $1 ORDER BY block_time`, orderID)
	if err != nil {
		return nil, fmt.Errorf("deposits: listing deposits for order %d: %w", orderID, err)
	}
	defer rows.Close()
	var out []Deposit
	for rows.Next() {
		var d Deposit
		var amount int64
		if err := rows.Scan(&d.TxID, &d.Address, &d.OrderID, &amount, &d.SenderAddress, &d.Status, &d.Note); err != nil {
			return nil, fmt.Errorf("deposits: listing deposits for order %d: %w", orderID, err)
		}
		d.Amount = money.Amount(amount)
		out = append(out, d)
	}
	return out, rows.Err()
}
