-- The transaction the vendor paid the customer in, as the vendor reports
-- it at completion: the customer's proof of delivery on the other chain,
-- kept with the rest of the order's record.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE relay_legs ADD COLUMN payout_tx_id text null;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE relay_legs DROP COLUMN payout_tx_id;
-- +goose StatementEnd
