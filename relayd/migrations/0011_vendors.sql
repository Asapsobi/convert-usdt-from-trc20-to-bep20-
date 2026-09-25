-- The vendor registry: every vendor relayd can use, per service --
-- conversion (FixedFloat, ChangeNOW, SideShift) and TRON energy rental
-- (CatFee, ...) -- with what an administrator controls (enabled,
-- priority) and what relayd observes (health).
--
-- A vendor that fails (times out, errors, answers garbage) is taken out
-- of the pool for a growing back-off and returned automatically once its
-- back-off passes and a call succeeds. A vendor refusing one particular
-- request (amount too small, bad address) is not a failure: the next
-- vendor is simply tried.
--
-- Credentials stay in the environment, never in this table.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE vendors (
    service               text not null CHECK (service IN ('conversion', 'energy')),
    name                  text not null,
    enabled               boolean not null default true,
    priority              int not null default 100,
    consecutive_failures  int not null default 0,
    unavailable_until     timestamptz null,
    last_error            text null,
    last_error_at         timestamptz null,
    last_success_at       timestamptz null,
    updated_at            timestamptz not null default now(),
    primary key (service, name)
);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO settings (key, value) VALUES
    ('vendor_selection', '{"conversion": "best_rate", "energy": "cheapest"}');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM settings WHERE key = 'vendor_selection';
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE vendors;
-- +goose StatementEnd
