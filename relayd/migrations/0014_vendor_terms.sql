-- Each vendor's terms, kept with its health in the registry: what the
-- vendor pays us (a referral/revenue share, in basis points of what we
-- send it) and an administrator's notes -- so vendors can be chosen by our
-- revenue as well as by the customer's rate or a fixed priority.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE vendors
    ADD COLUMN revenue_bps int not null default 0 CHECK (revenue_bps >= 0 AND revenue_bps < 10000),
    ADD COLUMN notes       text not null default '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE vendors DROP COLUMN revenue_bps, DROP COLUMN notes;
-- +goose StatementEnd
