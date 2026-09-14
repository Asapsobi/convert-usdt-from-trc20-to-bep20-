-- Model D's own B2C channel (docs/01-strategy/model-d-model-f-product-separation.md,
-- 14 Sep 2026 decision): retail_customers/retail_sessions are the end-user
-- identity/session tables alongside the existing B2B customers table --
-- a separate identity space, never merged into customers (a retail
-- customer has no API key, no webhook URL, no rate-limit override; a
-- B2B customer has no password).
--
-- quotes/gateway_orders both move from "owned by exactly one customer"
-- to "owned by exactly one of a customer OR a retail_customer" --
-- customer_id becomes nullable, retail_customer_id is added, and a
-- CHECK enforces exactly one is ever set. This is the same "exactly one
-- of X/Y" ownership-split pattern s1's own migration 0005
-- (signing_requests.slot_id/bsc_deposit_index) already uses for an
-- analogous problem: one unified table/pipeline serving two owner
-- kinds, rather than forking quotes/orders into parallel B2B/B2C
-- copies that could drift.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE retail_customers (
    id              bigint generated always as identity primary key,
    email           text not null unique,
    password_hash   text not null,
    status          text not null check (status in ('active', 'suspended')) default 'active',
    created_at      timestamptz not null default now(),
    updated_at      timestamptz not null default now()
);
-- +goose StatementEnd

-- +goose StatementBegin
-- session_token_hash, never the raw token -- mirrors customers.api_key_hash's
-- own "hash only, raw value shown exactly once at issuance" discipline.
-- A session is revocable (logout) and has a real expiry, unlike a B2B
-- API key -- retail_sessions is a much shorter-lived credential by
-- design, matching a web session rather than a service-to-service key.
CREATE TABLE retail_sessions (
    id                  bigint generated always as identity primary key,
    retail_customer_id  bigint not null references retail_customers(id),
    session_token_hash  text not null unique,
    created_at          timestamptz not null default now(),
    expires_at          timestamptz not null,
    revoked_at          timestamptz
);

CREATE INDEX idx_retail_sessions_customer_id ON retail_sessions(retail_customer_id);
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE quotes ALTER COLUMN customer_id DROP NOT NULL;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE quotes ADD COLUMN retail_customer_id bigint null references retail_customers(id);
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE quotes ADD CONSTRAINT quotes_exactly_one_owner
    CHECK ((customer_id IS NOT NULL) <> (retail_customer_id IS NOT NULL));
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX idx_quotes_retail_customer_id ON quotes(retail_customer_id);
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE gateway_orders ALTER COLUMN customer_id DROP NOT NULL;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE gateway_orders ADD COLUMN retail_customer_id bigint null references retail_customers(id);
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE gateway_orders ADD CONSTRAINT gateway_orders_exactly_one_owner
    CHECK ((customer_id IS NOT NULL) <> (retail_customer_id IS NOT NULL));
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX idx_gateway_orders_retail_customer_id ON gateway_orders(retail_customer_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX idx_gateway_orders_retail_customer_id;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE gateway_orders DROP CONSTRAINT gateway_orders_exactly_one_owner;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE gateway_orders DROP COLUMN retail_customer_id;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE gateway_orders ALTER COLUMN customer_id SET NOT NULL;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX idx_quotes_retail_customer_id;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE quotes DROP CONSTRAINT quotes_exactly_one_owner;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE quotes DROP COLUMN retail_customer_id;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE quotes ALTER COLUMN customer_id SET NOT NULL;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE retail_sessions;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE retail_customers;
-- +goose StatementEnd
