//go:build integration

package stats_test

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"relayd/internal/db"
	"relayd/internal/stats"
)

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	url := os.Getenv("RELAYD_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("RELAYD_TEST_DATABASE_URL not set; skipping integration test")
	}
	_, thisFile, _, _ := runtime.Caller(0)
	sqlDB, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(sqlDB, filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	pool, err := db.Open(context.Background(), db.Config{DatabaseURL: url})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// The overview's numbers move by exactly what new orders, top-ups and
// rentals add -- measured as a difference, since other tests share the
// database.
func TestCollect_CountsOrdersProfitAndCosts(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	now := time.Now()
	before, err := stats.Collect(ctx, pool, now)
	if err != nil {
		t.Fatal(err)
	}

	base := now.UnixNano() % 1_000_000_000
	leg := func(n int, status string, created, updated time.Time, received, profit *int64) string {
		id := fmt.Sprintf("stats-test-%d-%d", base, n)
		if _, err := pool.Exec(ctx, `
			INSERT INTO relay_legs (external_id, order_id, direction, status, customer_id, destination_address, deposit_address,
				amount_in, amount_in_asset, amount_out_expected, amount_out_expected_asset, received_amount, profit_amount, created_at, updated_at)
			VALUES ($1, $2, 'BEP20_TO_TRC20', $3, 'c', 'TDest', '0xDep', 2000000, 'USDT_BEP20', 1600000, 'USDT_TRC20', $4, $5, $6, $7)`,
			id, base*10+int64(n), status, received, profit, created, updated); err != nil {
			t.Fatalf("inserting leg %d: %v", n, err)
		}
		return id
	}
	i64 := func(v int64) *int64 { return &v }
	settled := leg(1, "SETTLED", now, now, i64(2_000_000), i64(5_000))
	lost := leg(2, "UNRECOVERABLE", now.Add(-10*24*time.Hour), now.Add(-10*24*time.Hour), i64(1_000_000), nil)
	stuck := leg(3, "FORWARDING", now.Add(-2*time.Hour), now.Add(-2*time.Hour), nil, nil)
	waiting := leg(4, "AWAITING_DEPOSIT", now, now, nil, nil)

	txn := 0
	transfer := func(purpose, asset, status string, amount int64) {
		txn++
		chain, nonce, expires := "BSC", any(int64(txn)), any(nil)
		if asset == "TRX" {
			chain, nonce, expires = "TRON", nil, now
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO transfer_attempts (external_id, purpose, chain, from_address, to_address, amount, asset, status,
				unsigned_tx, digest, signature, tx_hash, evm_nonce, tron_expires_at)
			VALUES ($1, $2, $3, 'a', 'b', $4, $5, $6, '\x00', $7, $8, $9, $10, $11)`,
			settled, purpose, chain, amount, asset, status, make([]byte, 32), make([]byte, 65),
			fmt.Sprintf("stats-tx-%d-%d", base, txn), nonce, expires); err != nil {
			t.Fatalf("inserting transfer: %v", err)
		}
	}
	transfer("GAS_TOPUP", "BNB", "CONFIRMED", 10_000_000_000_000) // 0.00001 BNB
	transfer("GAS_TOPUP", "BNB", "FAILED", 99_000_000_000_000)    // never counted
	transfer("TRX_TOPUP", "TRX", "CONFIRMED", 1_000)              // 0.001 TRX
	for _, r := range []struct {
		status string
		cost   int64
	}{{"CONFIRMED", 3_908_550}, {"FAILED", 1_000_000}} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO resource_rentals (job, purpose, address, resource, units, idempotency_key, status, cost_sun)
			VALUES ($1, 'FORWARD', 'TAddr', 'ENERGY', 130000, $2, $3, $4)`, settled, fmt.Sprintf("k-%d-%s", base, r.status), r.status, r.cost); err != nil {
			t.Fatalf("inserting rental: %v", err)
		}
	}

	after, err := stats.Collect(ctx, pool, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}

	for status, want := range map[string]int{"SETTLED": 1, "UNRECOVERABLE": 1, "FORWARDING": 1, "AWAITING_DEPOSIT": 1} {
		if got := after.OrdersByStatus[status] - before.OrdersByStatus[status]; got != want {
			t.Errorf("orders %s: +%d, want +%d", status, got, want)
		}
	}
	day, all := period(t, before, after, "24h"), period(t, before, after, "all")
	checks := []struct {
		name      string
		got, want string
	}{
		{"24h volume", day.Volume, "2"}, {"24h profit", day.Profit, "0.005"},
		{"24h gas", day.GasBNB, "0.00001"}, {"24h TRX", day.TRX, "0.001"}, {"24h energy", day.EnergyTRX, "3.90855"},
		{"all volume", all.Volume, "3"}, {"all profit", all.Profit, "0.005"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: +%s, want +%s", c.name, c.got, c.want)
		}
	}
	if day.Orders != 3 || day.Settled != 1 || all.Orders != 4 {
		t.Errorf("order counts: 24h +%d (settled +%d), all +%d; want +3 (+1), +4", day.Orders, day.Settled, all.Orders)
	}

	flagged := map[string]bool{}
	for _, a := range after.Attention {
		flagged[a.ExternalID] = true
	}
	if !flagged[lost] || !flagged[stuck] {
		t.Errorf("attention missed the unrecoverable or the stuck order: %+v", after.Attention)
	}
	if flagged[settled] || flagged[waiting] {
		t.Errorf("attention flagged a healthy order: %+v", after.Attention)
	}
}

// period returns after minus before for the named window.
func period(t *testing.T, before, after stats.Stats, name string) stats.Period {
	t.Helper()
	find := func(s stats.Stats) stats.Period {
		for _, p := range s.Periods {
			if p.Name == name {
				return p
			}
		}
		t.Fatalf("no %s period", name)
		return stats.Period{}
	}
	b, a := find(before), find(after)
	sub := func(x, y string) string {
		rx, _ := new(big.Rat).SetString(x)
		ry, _ := new(big.Rat).SetString(y)
		d := new(big.Rat).Sub(rx, ry)
		s := d.FloatString(18)
		for len(s) > 0 && s[len(s)-1] == '0' {
			s = s[:len(s)-1]
		}
		if len(s) > 0 && s[len(s)-1] == '.' {
			s = s[:len(s)-1]
		}
		return s
	}
	return stats.Period{Name: name, Orders: a.Orders - b.Orders, Settled: a.Settled - b.Settled,
		Volume: sub(a.Volume, b.Volume), Profit: sub(a.Profit, b.Profit),
		GasBNB: sub(a.GasBNB, b.GasBNB), TRX: sub(a.TRX, b.TRX), EnergyTRX: sub(a.EnergyTRX, b.EnergyTRX)}
}
