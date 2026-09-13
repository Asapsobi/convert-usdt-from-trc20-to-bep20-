-- R5 (refund path), first slice: adds the three refund-related statuses
-- from docs/02-architecture/model-f-relay-architecture.md's own state
-- diagram (REFUND_PENDING, REFUNDED, UNRECOVERABLE) and a column to
-- record the refund broadcast's own on-chain transaction id, mirroring
-- forward_tx_id's own role for the forward leg.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE relay_legs DROP CONSTRAINT relay_legs_status_check;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE relay_legs ADD CONSTRAINT relay_legs_status_check
    CHECK (status IN ('AWAITING_DEPOSIT', 'FORWARDING', 'FORWARDED', 'SETTLED', 'FAILED',
                       'REFUND_PENDING', 'REFUNDED', 'UNRECOVERABLE'));
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE relay_legs ADD COLUMN refund_tx_id text null;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE relay_legs DROP COLUMN refund_tx_id;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE relay_legs DROP CONSTRAINT relay_legs_status_check;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE relay_legs ADD CONSTRAINT relay_legs_status_check
    CHECK (status IN ('AWAITING_DEPOSIT', 'FORWARDING', 'FORWARDED', 'SETTLED', 'FAILED'));
-- +goose StatementEnd
