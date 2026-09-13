-- A deposit that finalized on-chain for an order C1 no longer considers
-- open. Mirrors depositwatcher/migrations/0004_orphaned_deposits.sql,
-- except unique on tx_id alone (a TRC20 transfer's natural key here,
-- not tx_hash+log_index -- see internal/orphaned's own package doc
-- comment).

-- +goose Up
-- +goose StatementBegin
CREATE TABLE orphaned_deposits (
    id                       bigserial primary key,
    order_id                 bigint not null,
    external_id              text not null,
    tx_id                    text not null,
    amount                   numeric(38,0) not null,
    detected_at              timestamptz not null default now(),
    order_state_at_detection text not null,
    resolution               text null,
    resolved_at              timestamptz null,
    resolved_by              text null,
    unique (tx_id)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE orphaned_deposits;
-- +goose StatementEnd
