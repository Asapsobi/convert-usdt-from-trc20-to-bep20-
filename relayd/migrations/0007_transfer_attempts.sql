-- transfer_attempts: every on-chain transfer relayd builds for a relay
-- leg (the forward to the vendor, or a refund to the depositor), written
-- to the database BEFORE it is signed and sent. Before this table the
-- in-flight transaction lived only in process memory, so a restart
-- between broadcasting and confirming forgot a transfer that could still
-- land -- and the next attempt, or a timeout refund, spent the same
-- deposit a second time.
--
-- Lifecycle: BUILT (unsigned, cannot land) -> SIGNED (signature stored;
-- may or may not have reached a node) -> BROADCAST (a node accepted it)
-- -> CONFIRMED | FAILED (executed and reverted, funds did not move) |
-- DROPPED (provably can never land). A BUILT attempt that is never sent
-- becomes ABANDONED. At most one attempt per leg and purpose is ever
-- open, so a new transaction is only built once every earlier one is
-- settled one way or the other.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE transfer_attempts (
    id                    bigserial primary key,
    external_id           text not null REFERENCES relay_legs (external_id),
    purpose               text not null CHECK (purpose IN ('FORWARD', 'REFUND')),
    chain                 text not null CHECK (chain IN ('BSC', 'TRON')),
    from_address          text not null,
    to_address            text not null,
    amount                numeric(38,0) not null CHECK (amount > 0),
    asset                 text not null CHECK (asset IN ('USDT_BEP20', 'USDT_TRC20')),
    status                text not null
        CHECK (status IN ('BUILT', 'SIGNED', 'BROADCAST', 'CONFIRMED', 'FAILED', 'DROPPED', 'ABANDONED')),
    unsigned_tx           bytea not null,
    digest                bytea not null CHECK (length(digest) = 32),
    signature             bytea null CHECK (signature IS NULL OR length(signature) = 65),
    tx_hash               text null,
    evm_nonce             bigint null,
    tron_expires_at       timestamptz null,
    broadcast_count       int not null default 0,
    last_broadcast_at     timestamptz null,
    last_broadcast_error  text null,
    nonce_consumed_since  timestamptz null,
    stuck_alerted_at      timestamptz null,
    failure_reason        text null,
    created_at            timestamptz not null default now(),
    updated_at            timestamptz not null default now(),
    CHECK (status IN ('BUILT', 'ABANDONED') OR (signature IS NOT NULL AND tx_hash IS NOT NULL)),
    CHECK ((chain = 'BSC') = (evm_nonce IS NOT NULL)),
    CHECK ((chain = 'TRON') = (tron_expires_at IS NOT NULL))
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX transfer_attempts_one_open
    ON transfer_attempts (external_id, purpose)
    WHERE status IN ('BUILT', 'SIGNED', 'BROADCAST');
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX transfer_attempts_one_confirmed
    ON transfer_attempts (external_id, purpose)
    WHERE status = 'CONFIRMED';
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX transfer_attempts_tx_hash
    ON transfer_attempts (tx_hash)
    WHERE tx_hash IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE transfer_attempts;
-- +goose StatementEnd
