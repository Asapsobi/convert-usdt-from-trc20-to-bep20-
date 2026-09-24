-- Records which real Privy (privy.io) Server Wallet backs each per-order
-- deposit-signing index, for whichever chain(s) have moved off local
-- in-process BIP32 custody (internal/kmssign/bscdeposit.go/trondeposit.go's
-- own PRODUCTION CAVEAT) onto real custody-as-a-service. A Privy wallet
-- is signing-capable custody the instant POST /v1/wallets mints it --
-- this table is exactly as custody-sensitive as s1_slot_keys, and lives
-- in the one module allowed to hold anything signing-capable (see
-- internal/kmssign's own package doc comment).
--
-- chain_type is a discriminator, not a TRON-only table, even though this
-- migration's own first real caller (internal/kmssign/privytron.go) is
-- TRON-only -- bsc_deposit_index/tron_deposit_index are already separate
-- columns elsewhere in this schema (0005/0006) specifically to avoid
-- ever deriving from the wrong key for a colliding index; the same
-- collision risk applies here without a discriminator once an EVM/BSC
-- Privy backend is added later.
--
-- public_key is nullable: Privy's own wallet-creation response reports a
-- public_key for chain_type=tron wallets, but NOT for chain_type=ethereum
-- ones (only an address) -- confirmed empirically via cmd/privy-probe,
-- not a placeholder for data we simply haven't backfilled yet.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE s1_privy_deposit_wallets (
    chain_type      text not null CHECK (chain_type IN ('tron', 'ethereum')),
    deposit_index   bigint not null CHECK (deposit_index >= 0 AND deposit_index < 2147483648),
    privy_wallet_id text not null unique,
    address         text not null unique,
    public_key      bytea,
    created_at      timestamptz not null default now(),
    PRIMARY KEY (chain_type, deposit_index)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE s1_privy_deposit_wallets;
-- +goose StatementEnd
