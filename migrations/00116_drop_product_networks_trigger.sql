-- +goose Up
-- The transitional trigger from 00113 only kept the pre-D60 binary (0.4.0) working while that
-- migration deployed. Production runs 0.6.0 and every binary writes the network row itself.
DROP TRIGGER IF EXISTS product_payout_items_network ON product_payout_items;
DROP FUNCTION IF EXISTS product_networks_for_payout();

-- +goose Down
-- +goose StatementBegin
CREATE FUNCTION product_networks_for_payout() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    INSERT INTO product_networks (product_id, merchant_id, livemode, chain_id, accept)
    VALUES (NEW.product_id, NEW.merchant_id, NEW.livemode, NEW.chain_id, 'all')
    ON CONFLICT (product_id, chain_id) DO NOTHING;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER product_payout_items_network BEFORE INSERT ON product_payout_items
    FOR EACH ROW EXECUTE FUNCTION product_networks_for_payout();
