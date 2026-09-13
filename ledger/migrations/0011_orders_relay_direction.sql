-- A real gap found while wiring Model F's own RELAY tier through this
-- package (not caught by the earlier "Tier is purely additive" check,
-- which only grepped for switches on Tier -- this is a switch on ASSET
-- that Tier drives implicitly): orders.amount_in/amount_out/fee_units/
-- network_fee_units are stored as bare numeric(38,0) with NO asset
-- column at all. Every existing tier (DIRECT/STANDARD/SWEEP) is a
-- fixed-direction corridor -- amount_in is always USDT_BEP20, the other
-- three always USDT_TRC20 -- so the asset was safely hard-coded on both
-- the write path (Create) and the read path (scanOrder). Model F's own
-- RELAY tier is bidirectional by design (TRC20->BEP20 is the PRIMARY
-- example in docs/03-build/model-f-relay-build-prompts.md's own happy
-- flow, not an edge case), so a RELAY order needs to record which
-- direction it actually is, or scanOrder would silently mislabel a
-- TRC20-in/BEP20-out order's own amounts as the other way around.
--
-- relay_direction is NULL for every non-RELAY order (the asset pairing
-- for those is still the fixed, implicit one) and required for RELAY
-- orders -- enforced by the CHECK below, tying the invariant directly to
-- the schema rather than trusting every future INSERT to remember it,
-- the same posture as 0002's normal_side_matches_type CHECK.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE orders ADD COLUMN relay_direction text
    CHECK (relay_direction IS NULL OR relay_direction IN ('BEP20_TO_TRC20', 'TRC20_TO_BEP20'));
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE orders ADD CONSTRAINT orders_relay_direction_matches_tier
    CHECK ((tier = 'RELAY') = (relay_direction IS NOT NULL));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE orders DROP CONSTRAINT orders_relay_direction_matches_tier;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE orders DROP COLUMN relay_direction;
-- +goose StatementEnd
