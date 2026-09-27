// Package stats sums up relayd's books for the admin panel's overview:
// orders by status, volume, profit and network costs per period, and the
// orders an operator should look at.
package stats

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	"relayd/internal/db"
)

// Stats is the overview's numbers.
type Stats struct {
	GeneratedAt    time.Time      `json:"generated_at"`
	OrdersByStatus map[string]int `json:"orders_by_status"`
	Periods        []Period       `json:"periods"`
	Attention      []Attention    `json:"attention"`
}

// Period sums the orders created in one time window, and the network
// costs paid in it.
type Period struct {
	Name      string `json:"name"` // 24h, 7d, 30d, all
	Orders    int    `json:"orders"`
	Settled   int    `json:"settled"`
	Volume    string `json:"volume_usdt"` // USDT that arrived, both networks
	Profit    string `json:"profit_usdt"` // our fee, settled orders only
	GasBNB    string `json:"gas_bnb"`     // confirmed BNB top-ups to deposit wallets
	TRX       string `json:"trx"`         // confirmed TRX top-ups (activation, bandwidth)
	EnergyTRX string `json:"energy_trx"`  // confirmed energy rentals
}

// Attention is an order an operator should look at.
type Attention struct {
	ExternalID string    `json:"external_id"`
	Direction  string    `json:"direction"`
	Status     string    `json:"status"`
	Reason     string    `json:"reason"`
	Since      time.Time `json:"since"`
}

// StuckAfter is how long an order may sit forwarding before it counts as
// stuck.
const StuckAfter = 30 * time.Minute

var periods = []struct {
	name string
	back time.Duration // 0 means since the beginning
}{{"24h", 24 * time.Hour}, {"7d", 7 * 24 * time.Hour}, {"30d", 30 * 24 * time.Hour}, {"all", 0}}

// Collect reads the numbers as of now.
func Collect(ctx context.Context, q db.Queryer, now time.Time) (Stats, error) {
	out := Stats{GeneratedAt: now, OrdersByStatus: map[string]int{}}

	rows, err := q.Query(ctx, `SELECT status, count(*) FROM relay_legs GROUP BY status`)
	if err != nil {
		return Stats{}, fmt.Errorf("stats: counting orders: %w", err)
	}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			rows.Close()
			return Stats{}, fmt.Errorf("stats: counting orders: %w", err)
		}
		out.OrdersByStatus[status] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Stats{}, fmt.Errorf("stats: counting orders: %w", err)
	}

	for _, p := range periods {
		since := time.Time{}
		if p.back > 0 {
			since = now.Add(-p.back)
		}
		period := Period{Name: p.name}
		var volume, profit, gas, trx, energy string
		err := q.QueryRow(ctx, `
			SELECT count(*), count(*) FILTER (WHERE status = 'SETTLED'),
				coalesce(sum(received_amount), 0)::text,
				coalesce(sum(profit_amount) FILTER (WHERE status = 'SETTLED'), 0)::text
			FROM relay_legs WHERE created_at >= $1`, since).Scan(&period.Orders, &period.Settled, &volume, &profit)
		if err != nil {
			return Stats{}, fmt.Errorf("stats: summing orders for %s: %w", p.name, err)
		}
		err = q.QueryRow(ctx, `
			SELECT coalesce(sum(amount) FILTER (WHERE purpose = 'GAS_TOPUP' AND asset = 'BNB'), 0)::text,
				coalesce(sum(amount) FILTER (WHERE purpose = 'TRX_TOPUP' AND asset = 'TRX'), 0)::text
			FROM transfer_attempts WHERE status = 'CONFIRMED' AND created_at >= $1`, since).Scan(&gas, &trx)
		if err != nil {
			return Stats{}, fmt.Errorf("stats: summing top-ups for %s: %w", p.name, err)
		}
		err = q.QueryRow(ctx, `
			SELECT coalesce(sum(cost_sun), 0)::text FROM resource_rentals
			WHERE status = 'CONFIRMED' AND resource = 'ENERGY' AND created_at >= $1`, since).Scan(&energy)
		if err != nil {
			return Stats{}, fmt.Errorf("stats: summing energy for %s: %w", p.name, err)
		}
		period.Volume, period.Profit = units(volume, 6), units(profit, 6)
		period.GasBNB, period.TRX, period.EnergyTRX = units(gas, 18), units(trx, 6), units(energy, 6)
		out.Periods = append(out.Periods, period)
	}

	rows, err = q.Query(ctx, `
		SELECT external_id, direction, status, updated_at FROM relay_legs
		WHERE status IN ('FAILED', 'UNRECOVERABLE', 'REFUND_PENDING')
			OR (status IN ('FORWARDING', 'FORWARDED') AND updated_at < $1)
		ORDER BY updated_at`, now.Add(-StuckAfter))
	if err != nil {
		return Stats{}, fmt.Errorf("stats: finding orders needing attention: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a Attention
		if err := rows.Scan(&a.ExternalID, &a.Direction, &a.Status, &a.Since); err != nil {
			return Stats{}, fmt.Errorf("stats: finding orders needing attention: %w", err)
		}
		a.Reason = reason(a.Status)
		out.Attention = append(out.Attention, a)
	}
	return out, rows.Err()
}

func reason(status string) string {
	switch status {
	case "UNRECOVERABLE":
		return "money was sent but the exchange failed; needs an operator"
	case "FAILED":
		return "the order failed"
	case "REFUND_PENDING":
		return "a refund is in progress"
	default:
		return fmt.Sprintf("in progress for more than %d minutes", int(StuckAfter.Minutes()))
	}
}

// units renders a raw integer amount with decimals places, trimmed.
func units(raw string, decimals int) string {
	n, ok := new(big.Int).SetString(raw, 10)
	if !ok {
		return "0"
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	whole, frac := new(big.Int).QuoRem(n, scale, new(big.Int))
	digits := frac.String()
	for len(digits) < decimals {
		digits = "0" + digits
	}
	if digits = strings.TrimRight(digits, "0"); digits == "" {
		return whole.String()
	}
	return whole.String() + "." + digits
}
