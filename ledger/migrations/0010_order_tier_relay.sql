-- Adds Model F's own order tier (docs/02-architecture/
-- model-f-relay-architecture.md §5) to the existing order_tier ENUM
-- created in 0006. Nothing else in this migration -- the two new
-- account categories that tier's orders reference
-- (asset:relay:leg:<order_id>, revenue:relay_commission) need no schema
-- change at all: account codes are free-text with no DB-level whitelist
-- (see ledger/internal/accounts/code.go), created on demand via the
-- existing POST /v1/accounts, the same pattern C5's own slot accounts
-- already use.
--
-- Run outside this migration's own transaction wrapper: Postgres allows
-- ALTER TYPE ... ADD VALUE inside a transaction (since PG 12), but the
-- new value cannot be referenced by any statement in that SAME
-- transaction -- not a concern here since nothing else in this file
-- uses 'RELAY'.
--
-- No safe Down for this one: Postgres has no ALTER TYPE ... DROP VALUE.
-- Reversing this would mean recreating order_tier from scratch after
-- confirming no row references 'RELAY' -- an operational procedure, not
-- something a migration can safely automate (a real RELAY-tier order
-- could exist by the time anyone runs `migrate down`). Left as an
-- explicit no-op rather than a doomed DROP TYPE, the same posture this
-- project takes toward every other operational gap it can't safely
-- close in code (e.g. S1's own real KMS key-generation ceremony).

-- +goose Up
-- +goose StatementBegin
ALTER TYPE order_tier ADD VALUE 'RELAY';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1; -- no-op -- see this file's own header comment on why ADD VALUE can't be safely reversed here
-- +goose StatementEnd
