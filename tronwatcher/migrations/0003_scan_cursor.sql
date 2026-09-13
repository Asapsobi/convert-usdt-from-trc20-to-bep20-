-- The per-address scan cursor. Unlike
-- depositwatcher/migrations/0003_ingestion_cursor.sql's global block-
-- height cursor (BSC/eth_getLogs scans the whole chain, filtered by
-- contract+topic), tronwatcher scans per-ADDRESS
-- (TronGrid's own /v1/accounts/{address}/transactions/trc20, see
-- internal/chain/provider.go's own doc comment on why there is no TRON
-- equivalent of eth_getLogs) -- so the natural cursor lives on
-- watched_addresses itself, one per row, not one global row. NULL means
-- "never scanned yet"; a fresh WATCHING address scans from its own
-- assigned_at, never from the epoch.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE watched_addresses ADD COLUMN last_scanned_at timestamptz null;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE watched_addresses DROP COLUMN last_scanned_at;
-- +goose StatementEnd
