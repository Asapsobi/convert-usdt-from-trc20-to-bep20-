-- Sweeps move the profit relayd keeps in deposit wallets to the treasury.
-- Each deposit wallet keeps our profit from every order it served (only
-- the rest is forwarded); once a wallet's unswept profit reaches the
-- administrator's minimum, and no order is using it, relayd sends that
-- profit to the treasury in one transfer.
--
-- A sweep only ever moves what the books say is ours: the profit recorded
-- on the wallet's settled legs, less earlier sweeps. amount is fixed when
-- the sweep starts. At most one sweep per wallet is in flight.
--
-- The 'sweep' setting is administrator-managed; sweeping starts disabled
-- until the treasury is ready to pay a sweep's gas and energy.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE sweeps (
    id               bigserial primary key,
    chain            text not null CHECK (chain IN ('BSC', 'TRON')),
    from_address     text not null,
    deposit_index    bigint not null CHECK (deposit_index >= 0),
    to_address       text not null,
    amount           numeric(38,0) not null CHECK (amount > 0),
    asset            text not null CHECK (asset IN ('USDT_BEP20', 'USDT_TRC20')),
    status           text not null default 'PENDING' CHECK (status IN ('PENDING', 'CONFIRMED', 'FAILED')),
    tx_hash          text null,
    ledger_entry_id  bigint null,
    error            text null,
    created_at       timestamptz not null default now(),
    updated_at       timestamptz not null default now(),
    finished_at      timestamptz null
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX sweeps_one_pending_per_wallet ON sweeps (from_address) WHERE status = 'PENDING';
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX sweeps_by_wallet ON sweeps (from_address, id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX relay_legs_by_deposit_address ON relay_legs (deposit_address);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO settings (key, value) VALUES
    ('sweep', '{"enabled": false, "interval_minutes": 60, "min_amount": {"USDT_BEP20": "10", "USDT_TRC20": "50"}}');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM settings WHERE key = 'sweep';
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX relay_legs_by_deposit_address;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE sweeps;
-- +goose StatementEnd
