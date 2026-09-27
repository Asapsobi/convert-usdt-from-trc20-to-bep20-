-- Leasing a pool wallet prefers one that has received a deposit before,
-- so it looks deposits up by address.

-- +goose Up
-- +goose StatementBegin
CREATE INDEX deposits_by_address ON deposits (address);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX deposits_by_address;
-- +goose StatementEnd
