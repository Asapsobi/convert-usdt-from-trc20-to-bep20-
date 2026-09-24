-- Records, per relay leg, the exact BIP32 derivation index the leg's own
-- watcher (depositwatcher's addresses.Assign for BEP20_TO_TRC20,
-- tronwatcher's own equivalent for TRC20_TO_BEP20) used for this leg's
-- real, unique deposit address -- required so a leg's own forward
-- signing can request a signature from THAT exact per-order key (via
-- S1's RequestDepositSweepSignature/RequestTronDepositSweepSignature)
-- instead of relayd's own single, shared slot key, which never actually
-- held the customer's deposited funds. Nullable at the column level only
-- because this migration predates TRC20_TO_BEP20 legs also populating
-- it; both directions set it in practice today. Bound matches
-- depositwatcher's own watched_addresses.derivation_index CHECK exactly
-- (0002_watched_addresses.sql).

-- +goose Up
-- +goose StatementBegin
ALTER TABLE relay_legs ADD COLUMN deposit_derivation_index bigint null
    CHECK (deposit_derivation_index IS NULL OR (deposit_derivation_index >= 0 AND deposit_derivation_index < 2147483648));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE relay_legs DROP COLUMN deposit_derivation_index;
-- +goose StatementEnd
