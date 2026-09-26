-- Deposit wallets are now leased from a limited pool (see the watchers'
-- own pool migrations), so relayd must hand each wallet back when its leg
-- is done, and expire legs nobody paid for so their wallet isn't held
-- forever.
--
-- EXPIRED: the deposit window (plus a grace period) passed with no
-- deposit; the C1 order is expired too, so a payment arriving later is
-- recorded as orphaned instead of funding anything.
-- lease_released_at: when relayd told the watcher this leg's wallet is
-- free again (it then cools down before its next lease).

-- +goose Up
-- +goose StatementBegin
ALTER TABLE relay_legs DROP CONSTRAINT relay_legs_status_check;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE relay_legs ADD CONSTRAINT relay_legs_status_check
    CHECK (status IN ('AWAITING_DEPOSIT', 'FORWARDING', 'FORWARDED', 'SETTLED', 'FAILED',
                       'REFUND_PENDING', 'REFUNDED', 'UNRECOVERABLE', 'EXPIRED'));
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE relay_legs ADD COLUMN lease_released_at timestamptz null;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE relay_legs DROP COLUMN lease_released_at;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE relay_legs DROP CONSTRAINT relay_legs_status_check;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE relay_legs ADD CONSTRAINT relay_legs_status_check
    CHECK (status IN ('AWAITING_DEPOSIT', 'FORWARDING', 'FORWARDED', 'SETTLED', 'FAILED',
                       'REFUND_PENDING', 'REFUNDED', 'UNRECOVERABLE'));
-- +goose StatementEnd
