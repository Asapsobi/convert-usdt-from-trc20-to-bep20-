-- Extends signing_requests/signing_audit_log to cover a second kind of
-- signing authority alongside the 6 fixed TRON/EVM slot keys: per-order
-- BSC deposit addresses, signed via internal/kmssign's own new
-- BSCDepositKeys (see that package's own bscdeposit.go doc comment for
-- why this is a real, separate custody-model gap from KMS-mediated slot
-- signing, not a cosmetic difference).
--
-- Rather than a second, parallel signing_requests table, this reuses the
-- existing one: slot_id becomes nullable and a new bsc_deposit_index
-- column is added, with a CHECK that exactly one of the two is set per
-- row. That keeps ONE unified request/approval/audit pipeline -- the
-- custody-sensitive part this table exists for -- instead of forking
-- Approve/Reject/GetSignature/ListPending into two near-identical
-- copies for what both fundamentally are the same thing: a pending
-- signature over a digest, gated by the same approval-threshold policy,
-- audited the same way.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE signing_requests ALTER COLUMN slot_id DROP NOT NULL;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE signing_requests ADD COLUMN bsc_deposit_index bigint null;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE signing_requests ADD CONSTRAINT signing_requests_exactly_one_key_ref
    CHECK ((slot_id IS NOT NULL) <> (bsc_deposit_index IS NOT NULL));
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE signing_audit_log ALTER COLUMN slot_id DROP NOT NULL;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE signing_audit_log ADD COLUMN bsc_deposit_index bigint null;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE signing_audit_log ADD CONSTRAINT signing_audit_log_exactly_one_key_ref
    CHECK ((slot_id IS NOT NULL) <> (bsc_deposit_index IS NOT NULL));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE signing_audit_log DROP CONSTRAINT signing_audit_log_exactly_one_key_ref;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE signing_audit_log DROP COLUMN bsc_deposit_index;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE signing_audit_log ALTER COLUMN slot_id SET NOT NULL;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE signing_requests DROP CONSTRAINT signing_requests_exactly_one_key_ref;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE signing_requests DROP COLUMN bsc_deposit_index;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE signing_requests ALTER COLUMN slot_id SET NOT NULL;
-- +goose StatementEnd
