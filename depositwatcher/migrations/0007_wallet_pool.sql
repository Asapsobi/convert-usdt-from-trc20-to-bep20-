-- A limited, reusable pool of deposit wallets instead of a brand-new
-- wallet per order, plus durable deposit records.
--
-- pool_wallets is every deposit wallet this watcher manages: which exist,
-- which are active, and when each may next be handed out. Fewer wallets
-- means sweeping profit to treasury costs less.
--
-- watched_addresses keeps one row per order, but an order's row is now a
-- LEASE on a pool wallet: the same address is leased again and again,
-- one order at a time. A deposit belongs to the lease that was open at
-- its block time. After a lease ends the wallet cools down before its
-- next lease, so a late payment from the previous customer is recorded
-- as orphaned rather than credited to the next one.
--
-- deposits records every detected deposit before the scan cursor moves
-- past it -- before this table, a deposit detected but not yet reported
-- lived only in memory and was lost for good on a restart.

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
    tx_hash         text not null,
    log_index       int not null,
    address         text not null,
    order_id        bigint not null,
    external_id     text not null,
    customer_id     text not null,
    amount          numeric(38,0) not null,
    sender_address  text not null,
    height          bigint not null,
    block_time      timestamptz not null,
    classification  int not null,
    status          text not null default 'DETECTED'
        CHECK (status IN ('DETECTED', 'REPORTED', 'ORPHANED', 'DROPPED')),
    note            text null,
    detected_at     timestamptz not null default now(),
    updated_at      timestamptz not null default now(),
    primary key (tx_hash, log_index)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX deposits_detected ON deposits (height) WHERE status = 'DETECTED';
-- +goose StatementEnd

-- +goose Down
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
