-- relay_legs: one row per Model F relay order, correlated to C1's own
-- order via external_id (not new C1 columns -- services in this repo
-- talk to each other over HTTP, never a shared database; see
-- docs/02-architecture/model-f-relay-architecture.md §5's own note on
-- exactly this point). Own database, own Postgres, same convention as
-- every other service here.
--
-- upstream_provider_name/upstream_order_id together are unique whenever
-- both are set -- docs/03-build/model-f-relay-build-prompts.md's own R3
-- acceptance criterion: "a duplicate CreateOrder response for the same
-- logical order must be rejected, not silently overwritten."

-- +goose Up
-- +goose StatementBegin
CREATE TABLE relay_legs (
    id                         bigserial primary key,
    external_id                text not null unique,
    order_id                   bigint not null,
    direction                  text not null
        CHECK (direction IN ('TRC20_TO_BEP20', 'BEP20_TO_TRC20')),
    status                     text not null
        CHECK (status IN ('AWAITING_DEPOSIT', 'FORWARDING', 'FORWARDED', 'SETTLED', 'FAILED')),
    customer_id                text not null,
    destination_address        text not null,
    deposit_address            text not null,
    amount_in                  numeric(38,0) not null,
    amount_in_asset            text not null CHECK (amount_in_asset IN ('USDT_BEP20', 'USDT_TRC20')),
    amount_out_expected        numeric(38,0) not null,
    amount_out_expected_asset  text not null CHECK (amount_out_expected_asset IN ('USDT_BEP20', 'USDT_TRC20')),
    amount_out_actual          numeric(38,0) null,
    upstream_provider_name     text null,
    upstream_order_id          text null,
    upstream_deposit_address   text null,
    forward_tx_id              text null,
    created_at                 timestamptz not null default now(),
    updated_at                 timestamptz not null default now()
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX relay_legs_upstream_order_unique
    ON relay_legs (upstream_provider_name, upstream_order_id)
    WHERE upstream_order_id IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE relay_legs;
-- +goose StatementEnd
