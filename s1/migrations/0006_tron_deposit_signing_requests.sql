-- Extends signing_requests/signing_audit_log (migration 0005) to cover a
-- THIRD kind of signing authority alongside the 6 fixed slot keys and
-- per-order BSC deposit addresses: per-order TRON deposit addresses,
-- signed via internal/kmssign's own new TronDepositKeys (see that
-- package's own trondeposit.go doc comment). Migration 0005's own
-- two-way XOR CHECK (exactly one of slot_id/bsc_deposit_index) no longer
-- describes the real constraint once a third mutually-exclusive column
-- exists, so it's dropped and replaced with Postgres's own num_nonnulls
-- idiom for "exactly one of three."

-- +goose Up
-- +goose StatementBegin
ALTER TABLE signing_requests ADD COLUMN tron_deposit_index bigint null;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE signing_requests DROP CONSTRAINT signing_requests_exactly_one_key_ref;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE signing_requests ADD CONSTRAINT signing_requests_exactly_one_key_ref
    CHECK (num_nonnulls(slot_id, bsc_deposit_index, tron_deposit_index) = 1);
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE signing_audit_log ADD COLUMN tron_deposit_index bigint null;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE signing_audit_log DROP CONSTRAINT signing_audit_log_exactly_one_key_ref;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE signing_audit_log ADD CONSTRAINT signing_audit_log_exactly_one_key_ref
    CHECK (num_nonnulls(slot_id, bsc_deposit_index, tron_deposit_index) = 1);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE signing_audit_log DROP CONSTRAINT signing_audit_log_exactly_one_key_ref;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE signing_audit_log ADD CONSTRAINT signing_audit_log_exactly_one_key_ref
    CHECK ((slot_id IS NOT NULL) <> (bsc_deposit_index IS NOT NULL));
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE signing_audit_log DROP COLUMN tron_deposit_index;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE signing_requests DROP CONSTRAINT signing_requests_exactly_one_key_ref;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE signing_requests ADD CONSTRAINT signing_requests_exactly_one_key_ref
    CHECK ((slot_id IS NOT NULL) <> (bsc_deposit_index IS NOT NULL));
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE signing_requests DROP COLUMN tron_deposit_index;
-- +goose StatementEnd
