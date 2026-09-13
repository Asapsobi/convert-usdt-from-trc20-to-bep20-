-- The address book: one watch-only TRC20 address per order, its
-- lifecycle status, and the sequence that guarantees a derivation index
-- is never reused. Mirrors
-- depositwatcher/migrations/0002_watched_addresses.sql exactly -- the
-- schema is chain-agnostic.

-- +goose Up
-- +goose StatementBegin
CREATE TYPE address_status AS ENUM ('WATCHING', 'FUNDED', 'RETIRED');
-- +goose StatementEnd

-- +goose StatementBegin
-- MAXVALUE is 2^31 - 1: internal/addresses.DeriveAddress rejects any
-- index at or above 2^31, matching depositwatcher's own reasoning.
CREATE SEQUENCE watched_addresses_derivation_index_seq
    START WITH 0
    MINVALUE 0
    MAXVALUE 2147483647;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE watched_addresses (
    id                bigserial primary key,
    address           text not null unique,
    derivation_index  bigint not null unique
        CHECK (derivation_index >= 0 AND derivation_index < 2147483648),
    order_id          bigint not null unique,   -- C1's internal order id
    external_id       text not null unique,
    customer_id       text not null,
    status            address_status not null default 'WATCHING',
    quoted_at         timestamptz not null,
    quote_expires_at  timestamptz not null,
    assigned_at       timestamptz not null default now(),
    retired_at        timestamptz null,
    retired_reason    text null                 -- settled | refunded | expired | superseded
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION watched_addresses_check_transition() RETURNS trigger AS $$
BEGIN
    IF (OLD.status, NEW.status) NOT IN (
        ('WATCHING', 'FUNDED'),
        ('WATCHING', 'RETIRED'),
        ('FUNDED', 'RETIRED'),
        ('FUNDED', 'WATCHING')
    ) THEN
        RAISE EXCEPTION 'watched_addresses: illegal status transition % -> % for order %',
            OLD.status, NEW.status, OLD.order_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER watched_addresses_transition_check
    BEFORE UPDATE OF status ON watched_addresses
    FOR EACH ROW
    WHEN (OLD.status IS DISTINCT FROM NEW.status)
    EXECUTE FUNCTION watched_addresses_check_transition();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER watched_addresses_transition_check ON watched_addresses;
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION watched_addresses_check_transition();
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE watched_addresses;
-- +goose StatementEnd
-- +goose StatementBegin
DROP SEQUENCE watched_addresses_derivation_index_seq;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TYPE address_status;
-- +goose StatementEnd
