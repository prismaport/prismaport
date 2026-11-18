//go:build integration

package db_test

import (
	"testing"

	"github.com/prismaport/prismaport/migrations"
)

// 00113 (D60): every network a product pays becomes `all`, so what it offers is unchanged; a
// payout exists only on a taken network, dropping the network drops its payout, and a payout item
// written without a network (the binary serving during the deploy) takes it as `all`.
func TestMigration00113ProductNetworks(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool, m := chainOn(t, migrations.FS)
	if err := m.UpTo(ctx, 112); err != nil {
		t.Fatal(err)
	}
	seedMerchant(t, pool)
	exec(t, pool, `INSERT INTO wallets (id, merchant_id, livemode, family, label, address, verified, created_at) VALUES
		('wal_e', 'mer_1', true, 'evm', 'E', '0xE', true, now()), ('wal_e2', 'mer_1', true, 'evm', 'E2', '0xE2', true, now()),
		('wal_s', 'mer_1', true, 'solana', 'S', 'So1', true, now())`)
	product := func(id string) string {
		return `INSERT INTO products (id, merchant_id, livemode, name, price_type, amount_cents) VALUES ('` + id + `', 'mer_1', true, 'P', 'fixed', 100)`
	}
	tx(t, pool, product("prod_a"),
		`INSERT INTO product_payout_items VALUES ('prod_a', 'mer_1', true, 'base', 'evm', 0, 'wal_e', 6000)`,
		`INSERT INTO product_payout_items VALUES ('prod_a', 'mer_1', true, 'base', 'evm', 1, 'wal_e2', 4000)`,
		`INSERT INTO product_payout_items VALUES ('prod_a', 'mer_1', true, 'solana', 'solana', 0, 'wal_s', 10000)`)
	if err := m.UpTo(ctx, 113); err != nil {
		t.Fatal(err)
	}
	if got := scanText(t, pool, `SELECT string_agg(chain_id || ':' || accept || ':' || coalesce(array_to_string(assets, ','), '-'), ' ' ORDER BY chain_id)
		FROM product_networks WHERE product_id = 'prod_a'`); got != "base:all:- solana:all:-" {
		t.Fatalf("backfilled networks: %s", got)
	}

	mustFail(t, pool, "some without assets", `INSERT INTO product_networks VALUES ('prod_a', 'mer_1', true, 'x', 'some', NULL)`)
	mustFail(t, pool, "all with assets", `INSERT INTO product_networks VALUES ('prod_a', 'mer_1', true, 'x', 'all', '{USDC}')`)
	mustFail(t, pool, "none is no row", `INSERT INTO product_networks VALUES ('prod_a', 'mer_1', true, 'x', 'none', NULL)`)
	mustFail(t, pool, "empty list", `INSERT INTO product_networks VALUES ('prod_a', 'mer_1', true, 'x', 'some', '{}')`)
	mustFail(t, pool, "null element", `INSERT INTO product_networks VALUES ('prod_a', 'mer_1', true, 'x', 'some', '{USDC,NULL}')`)
	mustFail(t, pool, "five assets", `INSERT INTO product_networks VALUES ('prod_a', 'mer_1', true, 'x', 'some', '{A,B,C,D,E}')`)
	mustFail(t, pool, "other mode", `INSERT INTO product_networks VALUES ('prod_a', 'mer_1', false, 'x', 'all', NULL)`)
	exec(t, pool, `UPDATE product_networks SET accept = 'some', assets = '{USDC,USDT}' WHERE product_id = 'prod_a' AND chain_id = 'solana'`)

	// A payout item written without its network row takes the network as all.
	tx(t, pool, product("prod_b"), `INSERT INTO product_payout_items VALUES ('prod_b', 'mer_1', true, 'base', 'evm', 0, 'wal_e', 10000)`)
	if got := scanText(t, pool, `SELECT accept FROM product_networks WHERE product_id = 'prod_b' AND chain_id = 'base'`); got != "all" {
		t.Fatalf("network of a payout written without one: %s", got)
	}

	// Dropping a network drops its payout (the product still pays base).
	if err := txErr(pool, `DELETE FROM product_networks WHERE product_id = 'prod_a' AND chain_id = 'solana'`); err != nil {
		t.Fatal(err)
	}
	if got := scanText(t, pool, `SELECT count(*)::text FROM product_payout_items WHERE product_id = 'prod_a' AND chain_id = 'solana'`); got != "0" {
		t.Fatalf("payout left on a dropped network: %s", got)
	}
	// Dropping the last network leaves the product paying nothing, which the payout check refuses.
	if err := txErr(pool, `DELETE FROM product_networks WHERE product_id = 'prod_a'`); err == nil {
		t.Fatal("a product left paying no network was accepted")
	}

	if err := m.DownTo(ctx, 112); err != nil {
		t.Fatal(err)
	}
	if got := scanText(t, pool, `SELECT count(*)::text FROM product_payout_items WHERE product_id = 'prod_a'`); got != "2" {
		t.Fatalf("payout items after down: %s", got)
	}
}
