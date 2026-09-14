-- The stale-relay-leg reconciliation alarm
-- (docs/02-architecture/model-f-relay-architecture.md §5's own "an
-- account open after an hour is an operational alarm"): records when a
-- leg was first alerted on for sitting non-terminal too long, so the
-- alarm fires once per leg, not once per tick forever.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE relay_legs ADD COLUMN stale_alerted_at timestamptz null;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE relay_legs DROP COLUMN stale_alerted_at;
-- +goose StatementEnd
