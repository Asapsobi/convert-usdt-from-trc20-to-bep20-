-- The TRON side of the deposit-wallet pool, durable deposits, and a scan
-- cursor that can't skip deposits. See depositwatcher's own
-- 0007_wallet_pool.sql for the pool and lease design; it is identical
-- here, with TRON base58 addresses.
--
-- Fewer TRON wallets matter even more than on BSC: every wallet a sweep
-- touches needs its own energy and bandwidth, and each new wallet needs
-- activating (a TRX transfer) before it can send at all.
--
-- scan_cursors replaces watched_addresses.last_scanned_at. The old cursor
-- was the wall-clock time a scan started, but TronGrid's confirmed-only
-- index lags the chain by about a minute: a deposit confirmed after a
-- scan had already moved the cursor past its block time was never seen.
-- A scan now re-reads an overlap window behind its cursor, and the
-- deposits table makes re-reading the same transfer harmless.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE pool_wallets (
    address           text primary key,
    derivation_index  bigint not null unique
        CHECK (derivation_index >= 0 AND derivation_index < 2147483648),
    status            text not null default 'ACTIVE' CHECK (status IN ('ACTIVE', 'DISABLED')),
    available_after   timestamptz not null default now(),
    last_leased_at    timestamptz null,
    created_at        timestamptz not null default now()
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE pool_settings (
    id                     smallint primary key default 1 CHECK (id = 1),
    max_wallets            int not null default 10 CHECK (max_wallets >= 0),
    cooldown_after_use     interval not null default '30 minutes',
    cooldown_after_expiry  interval not null default '6 hours',
    updated_at             timestamptz not null default now()
);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO pool_settings DEFAULT VALUES;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE watched_addresses DROP CONSTRAINT watched_addresses_address_key;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE watched_addresses DROP CONSTRAINT watched_addresses_derivation_index_key;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX watched_addresses_one_open_lease
    ON watched_addresses (address) WHERE status <> 'RETIRED';
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX watched_addresses_by_address_time ON watched_addresses (address, assigned_at);
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE orphaned_deposits ADD COLUMN address text null;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE deposits (
    tx_id           text primary key,
    address         text not null,
    order_id        bigint not null,
    external_id     text not null,
    customer_id     text not null,
    amount          numeric(38,0) not null,
    sender_address  text not null,
    block_time      timestamptz not null,
    classification  int not null,
    status          text not null default 'DETECTED'
        CHECK (status IN ('DETECTED', 'REPORTED', 'ORPHANED', 'DROPPED')),
    note            text null,
    detected_at     timestamptz not null default now(),
    updated_at      timestamptz not null default now()
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE scan_cursors (
    address          text primary key,
    last_scanned_at  timestamptz not null
);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO scan_cursors (address, last_scanned_at)
SELECT DISTINCT ON (address) address, COALESCE(last_scanned_at, assigned_at)
FROM watched_addresses WHERE status <> 'RETIRED'
ORDER BY address, assigned_at DESC;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE scan_cursors;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE deposits;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE orphaned_deposits DROP COLUMN address;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX watched_addresses_by_address_time;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX watched_addresses_one_open_lease;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE watched_addresses ADD CONSTRAINT watched_addresses_derivation_index_key UNIQUE (derivation_index);
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE watched_addresses ADD CONSTRAINT watched_addresses_address_key UNIQUE (address);
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE pool_settings;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE pool_wallets;
-- +goose StatementEnd
