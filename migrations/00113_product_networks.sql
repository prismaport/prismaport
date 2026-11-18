-- +goose Up
-- The stablecoins a product takes per network (D60). One row per network
-- the product takes: `all` follows the merchant's accepted assets of that network (Settings ›
-- Networks), turned on later included; `some` takes only the listed assets, and a listed asset the
-- merchant turned off stays stored but is not offered until it is on again. A network the product
-- does not take has no row (and no payout items). What a checkout offers is computed, never
-- stored: the registry ∩ the merchant's accepted assets ∩ this row.
--
-- Every product taking a network today becomes `all` there, which is what it offered before this
-- migration, so nothing changes for anyone. Both tables are small (a few rows per product), so
-- the backfill and the foreign key run in this transaction.
SET LOCAL lock_timeout = '10s';

CREATE TABLE product_networks (
    product_id text NOT NULL,
    merchant_id text NOT NULL,
    livemode boolean NOT NULL,
    chain_id text NOT NULL,
    accept text NOT NULL,
    assets text[],
    CONSTRAINT product_networks_pkey PRIMARY KEY (product_id, chain_id),
    CONSTRAINT product_networks_chain_id_check CHECK (chain_id <> ''),
    CONSTRAINT product_networks_accept_check CHECK (accept IN ('all', 'some')),
    -- `some` lists 1 to 4 assets (a forwarder binds at most 4 tokens, D41); `all` lists none.
    CONSTRAINT product_networks_assets_check CHECK ((accept = 'some') = (assets IS NOT NULL)
        AND (assets IS NULL OR (cardinality(assets) BETWEEN 1 AND 4 AND array_position(assets, NULL) IS NULL))),
    CONSTRAINT product_networks_product_id_merchant_id_livemode_fkey FOREIGN KEY (product_id, merchant_id, livemode)
        REFERENCES products (id, merchant_id, livemode)
);

INSERT INTO product_networks (product_id, merchant_id, livemode, chain_id, accept)
SELECT DISTINCT product_id, merchant_id, livemode, chain_id, 'all' FROM product_payout_items;

-- A payout exists only on a network the product takes; dropping the network drops its payout.
ALTER TABLE product_payout_items ADD CONSTRAINT product_payout_items_network_fkey
    FOREIGN KEY (product_id, chain_id) REFERENCES product_networks (product_id, chain_id) ON DELETE CASCADE;

-- Transitional, for the binary that serves while this migration deploys: it writes payout items
-- without a network row. Such an item takes its network as `all` (what that binary offers). The
-- current binary writes the network first, so this never fires for it; a network row an older
-- binary leaves without payout items is a network the product does not take. Drop it once no
-- binary older than this migration runs.
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

-- +goose Down
DROP TRIGGER product_payout_items_network ON product_payout_items;
DROP FUNCTION product_networks_for_payout();
ALTER TABLE product_payout_items DROP CONSTRAINT product_payout_items_network_fkey;
DROP TABLE product_networks;
