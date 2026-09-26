-- Admin-managed settings (pricing first) and full per-leg money
-- tracking.
--
-- Pricing used to be a startup env var (RELAYD_FEE_BASIS_POINTS); an
-- administrator now sets it at runtime. Each leg snapshots the pricing it
-- was quoted under, so a change never re-prices an order a customer has
-- already accepted.
--
-- The tracking columns record what actually happened, not what was
-- quoted: the amount that really arrived (which the forward, the profit,
-- and any refund are all computed from), what we kept, what went to the
-- vendor, and what the vendor kept.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE settings (
    key         text primary key,
    value       jsonb not null,
    updated_at  timestamptz not null default now(),
    updated_by  text not null default 'migration'
);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO settings (key, value) VALUES
    ('pricing', '{"profit_bps": 25, "min_profit": "0", "min_amount_in": "5", "max_amount_in": "10000"}');
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE relay_legs
    ADD COLUMN customer_label     text null,
    ADD COLUMN profit_bps         int null CHECK (profit_bps IS NULL OR (profit_bps >= 0 AND profit_bps < 10000)),
    ADD COLUMN min_profit         numeric(38,0) null CHECK (min_profit IS NULL OR min_profit >= 0),
    ADD COLUMN received_amount    numeric(38,0) null CHECK (received_amount IS NULL OR received_amount > 0),
    ADD COLUMN sender_address     text null,
    ADD COLUMN profit_amount      numeric(38,0) null CHECK (profit_amount IS NULL OR profit_amount >= 0),
    ADD COLUMN forward_amount     numeric(38,0) null CHECK (forward_amount IS NULL OR forward_amount > 0),
    ADD COLUMN vendor_fee_amount  numeric(38,0) null;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE relay_legs
    DROP COLUMN customer_label, DROP COLUMN profit_bps, DROP COLUMN min_profit,
    DROP COLUMN received_amount, DROP COLUMN sender_address, DROP COLUMN profit_amount,
    DROP COLUMN forward_amount, DROP COLUMN vendor_fee_amount;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE settings;
-- +goose StatementEnd
