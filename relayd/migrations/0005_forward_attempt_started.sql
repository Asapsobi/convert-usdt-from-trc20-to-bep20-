-- Records the first time relayd ever notices a leg needs forwarding
-- (its own C1 order reached screened) -- the timestamp
-- refundStuckAwaitingDepositLegs needs to distinguish "no deposit has
-- arrived yet" (legitimately open-ended) from "a deposit arrived and
-- upstream.CreateOrder keeps failing" (stuck, refund it), a gap
-- refund.go's own doc comment already flagged before this migration.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE relay_legs ADD COLUMN forward_attempt_started_at timestamptz null;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE relay_legs DROP COLUMN forward_attempt_started_at;
-- +goose StatementEnd
