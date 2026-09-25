-- transfer_attempts now records every transaction relayd sends, not only a
-- leg's forward and refund: BNB gas top-ups and TRX top-ups (which also
-- activate a new TRON wallet) sent from the treasury, and sweeps from
-- deposit wallets to treasury. external_id becomes the attempt's job key:
-- a leg's external id for its forward/refund, or a key like
-- "gas:<wallet>:<job>" for the rest -- so it no longer references
-- relay_legs.
--
-- At most one open attempt per sending address (index below): two
-- concurrent transactions from one address would race on its nonce (BSC)
-- and on its balance (both chains).
--
-- resource_rentals records every TRON energy rental before the vendor is
-- asked for it, with its cost: a crash mid-rental never buys twice, and
-- each order's resource costs are on record.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE transfer_attempts DROP CONSTRAINT transfer_attempts_external_id_fkey;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE transfer_attempts DROP CONSTRAINT transfer_attempts_purpose_check;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE transfer_attempts ADD CONSTRAINT transfer_attempts_purpose_check
    CHECK (purpose IN ('FORWARD', 'REFUND', 'GAS_TOPUP', 'TRX_TOPUP', 'SWEEP'));
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE transfer_attempts DROP CONSTRAINT transfer_attempts_asset_check;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE transfer_attempts ADD CONSTRAINT transfer_attempts_asset_check
    CHECK (asset IN ('USDT_BEP20', 'USDT_TRC20', 'BNB', 'TRX'));
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX transfer_attempts_open_by_sender ON transfer_attempts (from_address)
    WHERE status IN ('BUILT', 'SIGNED', 'BROADCAST');
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE resource_rentals (
    id               bigserial primary key,
    job              text not null,
    purpose          text not null,
    address          text not null,
    resource         text not null CHECK (resource IN ('ENERGY', 'BANDWIDTH')),
    units            bigint not null CHECK (units > 0),
    idempotency_key  text not null unique,
    provider         text null,
    status           text not null default 'PENDING' CHECK (status IN ('PENDING', 'CONFIRMED', 'FAILED')),
    cost_sun         bigint null,
    error            text null,
    created_at       timestamptz not null default now(),
    updated_at       timestamptz not null default now()
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX resource_rentals_by_job ON resource_rentals (job, purpose, resource, id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE resource_rentals;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX transfer_attempts_open_by_sender;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE transfer_attempts DROP CONSTRAINT transfer_attempts_asset_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE transfer_attempts ADD CONSTRAINT transfer_attempts_asset_check CHECK (asset IN ('USDT_BEP20', 'USDT_TRC20'));
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE transfer_attempts DROP CONSTRAINT transfer_attempts_purpose_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE transfer_attempts ADD CONSTRAINT transfer_attempts_purpose_check CHECK (purpose IN ('FORWARD', 'REFUND'));
-- +goose StatementEnd
