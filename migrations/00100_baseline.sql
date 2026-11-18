-- Baseline: the schema the legacy migrations 00001-00046 (migrations/legacy) leave, as one file.
--
-- A fresh database starts here. A database still on the legacy chain is adopted instead: the
-- migrator applies the legacy migrations it is missing, checks that its schema matches
-- 00100_baseline.schema, and replaces the legacy rows of goose_db_version with this version
-- (internal/db/adopt.go). This file is never run there.
--
-- Generated from `pg_dump --schema-only` of a database migrated through the legacy chain and
-- regrouped by table, in the order the legacy chain created the tables (a table follows the
-- tables it references). TestBaselineMatchesLegacyChain checks that both build the same schema;
-- 00100_baseline.schema is that schema read back from the catalog.
--
-- Do not edit: it has run on shared databases. New migrations start at 00101.

-- +goose Up

-- Money amounts in token base units: whole numbers from 0 to 2^256-1. Unconstrained numeric
-- with checks, not numeric(78,0), which would round a fractional input before any check sees it.
CREATE DOMAIN base_units AS numeric
	CONSTRAINT base_units_check CHECK (((VALUE = trunc(VALUE)) AND (VALUE >= (0)::numeric) AND (VALUE <= '115792089237316195423570985008687907853269984665640564039457584007913129639935'::numeric)));

-- Trigger functions of the payout checks (each table lists its triggers after its indexes).

-- +goose StatementBegin
CREATE FUNCTION payout_check_customer_address() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    aid text;
    ch text;
    n int;
    total bigint;
    last int;
BEGIN
    IF TG_OP = 'DELETE' THEN
        aid := OLD.customer_address_id;
        ch := coalesce(to_jsonb(OLD) ->> 'chain_id', to_jsonb(OLD) ->> 'chain');
    ELSE
        aid := NEW.customer_address_id;
        ch := coalesce(to_jsonb(NEW) ->> 'chain_id', to_jsonb(NEW) ->> 'chain');
    END IF;
    SELECT count(*), coalesce(sum(bps), 0), coalesce(max(position), -1) INTO n, total, last
        FROM customer_address_payouts WHERE customer_address_id = aid AND chain_id = ch;
    IF n = 0 AND NOT EXISTS (SELECT 1 FROM customer_address_pay_to WHERE customer_address_id = aid AND chain = ch) THEN
        RETURN NULL;
    END IF;
    IF n < 1 OR n > 5 OR last <> n - 1 OR total <> 10000 THEN
        RAISE EXCEPTION 'customer address % pays % to 1 to 5 recipients at positions 0 to n-1 adding up to 10000 bps (% rows, % bps)',
            aid, ch, n, total USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION payout_check_product() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    pid text;
    net record;
BEGIN
    IF TG_TABLE_NAME = 'products' THEN
        pid := NEW.id;
    ELSIF TG_OP = 'DELETE' THEN
        pid := OLD.product_id;
    ELSE
        pid := NEW.product_id;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM products WHERE id = pid) THEN
        RETURN NULL;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM product_payout_items WHERE product_id = pid) THEN
        RAISE EXCEPTION 'product % pays no network', pid USING ERRCODE = 'check_violation';
    END IF;
    FOR net IN SELECT chain_id, count(*) AS n, sum(bps) AS total, max(position) AS last, count(DISTINCT family) AS families
               FROM product_payout_items WHERE product_id = pid GROUP BY chain_id LOOP
        IF net.n > 5 OR net.last <> net.n - 1 OR net.total <> 10000 OR net.families <> 1 THEN
            RAISE EXCEPTION 'product % pays % to 1 to 5 wallets of one family at positions 0 to n-1 adding up to 10000 bps (% items, % bps)',
                pid, net.chain_id, net.n, net.total USING ERRCODE = 'check_violation';
        END IF;
    END LOOP;
    IF EXISTS (SELECT 1 FROM product_payout_items i JOIN wallets w ON w.id = i.wallet_id WHERE i.product_id = pid AND w.archived_at IS NOT NULL) THEN
        RAISE EXCEPTION 'product % pays an archived wallet', pid USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION payout_check_session() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    sid text;
    ch text;
    n int;
    total bigint;
    last int;
BEGIN
    IF TG_OP = 'DELETE' THEN
        sid := OLD.session_id;
        ch := coalesce(to_jsonb(OLD) ->> 'chain_id', to_jsonb(OLD) ->> 'chain');
    ELSE
        sid := NEW.session_id;
        ch := coalesce(to_jsonb(NEW) ->> 'chain_id', to_jsonb(NEW) ->> 'chain');
    END IF;
    SELECT count(*), coalesce(sum(bps), 0), coalesce(max(position), -1) INTO n, total, last
        FROM session_payouts WHERE session_id = sid AND chain_id = ch;
    IF n = 0 AND NOT EXISTS (SELECT 1 FROM session_pay_to WHERE session_id = sid AND chain = ch) THEN
        RETURN NULL;
    END IF;
    IF n < 1 OR n > 5 OR last <> n - 1 OR total <> 10000 THEN
        RAISE EXCEPTION 'session % pays % to 1 to 5 recipients at positions 0 to n-1 adding up to 10000 bps (% rows, % bps)',
            sid, ch, n, total USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION payout_check_settlement() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    sid text;
    paid numeric;
    want numeric;
BEGIN
    IF TG_TABLE_NAME = 'settlements' THEN
        sid := NEW.id;
    ELSIF TG_OP = 'DELETE' THEN
        sid := OLD.settlement_id;
    ELSE
        sid := NEW.settlement_id;
    END IF;
    SELECT net INTO want FROM settlements WHERE id = sid;
    IF NOT FOUND THEN
        RETURN NULL;
    END IF;
    SELECT coalesce(sum(amount), 0) INTO paid FROM settlement_payouts WHERE settlement_id = sid;
    IF paid <> want THEN
        RAISE EXCEPTION 'settlement % pays out % but its net is %', sid, paid, want USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION payout_guard_archive() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.archived_at IS NOT NULL AND OLD.archived_at IS NULL AND EXISTS (SELECT 1 FROM product_payout_items WHERE wallet_id = NEW.id) THEN
        RAISE EXCEPTION 'wallet % is used by a product', NEW.id USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.address <> OLD.address OR NEW.family <> OLD.family OR NEW.merchant_id <> OLD.merchant_id OR NEW.livemode <> OLD.livemode THEN
        RAISE EXCEPTION 'a wallet address is immutable' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd

CREATE TABLE logos (
    id text NOT NULL,
    png bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT logos_id_check CHECK ((id ~ '^[0-9a-f]{32}$'::text)),
    CONSTRAINT logos_png_check CHECK (((octet_length(png) >= 1) AND (octet_length(png) <= 524288))),
    CONSTRAINT logos_pkey PRIMARY KEY (id)
);

CREATE TABLE merchants (
    id text NOT NULL,
    name text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    live_enabled_at timestamp with time zone,
    logo_id text,
    CONSTRAINT merchants_name_check CHECK (((length(name) >= 1) AND (length(name) <= 200))),
    CONSTRAINT merchants_pkey PRIMARY KEY (id),
    CONSTRAINT merchants_logo_id_fkey FOREIGN KEY (logo_id) REFERENCES logos(id)
);
CREATE INDEX merchants_logo_id ON merchants USING btree (logo_id) WHERE (logo_id IS NOT NULL);

CREATE TABLE api_keys (
    id text NOT NULL,
    merchant_id text NOT NULL,
    livemode boolean NOT NULL,
    key_hash bytea NOT NULL,
    key_prefix text NOT NULL,
    scopes text[] NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    revoked_at timestamp with time zone,
    name text,
    last_used_at timestamp with time zone,
    expires_at timestamp with time zone,
    replaced_by text,
    CONSTRAINT api_keys_expiry_after_creation CHECK (((expires_at IS NULL) OR (expires_at >= created_at))),
    CONSTRAINT api_keys_key_hash_check CHECK ((length(key_hash) = 32)),
    CONSTRAINT api_keys_name_check CHECK (((name IS NULL) OR ((length(name) >= 1) AND (length(name) <= 100)))),
    CONSTRAINT api_keys_not_self_replaced CHECK (((replaced_by IS NULL) OR (replaced_by <> id))),
    CONSTRAINT api_keys_rolled_together CHECK (((replaced_by IS NULL) = (expires_at IS NULL))),
    CONSTRAINT api_keys_scopes_check CHECK ((cardinality(scopes) > 0)),
    CONSTRAINT api_keys_key_hash_key UNIQUE (key_hash),
    CONSTRAINT api_keys_pkey PRIMARY KEY (id),
    CONSTRAINT api_keys_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES merchants(id),
    CONSTRAINT api_keys_replaced_by_fkey FOREIGN KEY (replaced_by) REFERENCES api_keys(id)
);
CREATE INDEX api_keys_expiring ON api_keys USING btree (expires_at) WHERE ((expires_at IS NOT NULL) AND (revoked_at IS NULL));
CREATE INDEX api_keys_merchant ON api_keys USING btree (merchant_id);
CREATE UNIQUE INDEX api_keys_replaced_by ON api_keys USING btree (replaced_by) WHERE (replaced_by IS NOT NULL);

CREATE TABLE merchant_assets (
    merchant_id text NOT NULL,
    livemode boolean NOT NULL,
    chain text NOT NULL,
    asset text NOT NULL,
    CONSTRAINT merchant_assets_pkey PRIMARY KEY (merchant_id, livemode, chain, asset),
    CONSTRAINT merchant_assets_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES merchants(id)
);

CREATE TABLE audit_log (
    id bigserial NOT NULL,
    merchant_id text,
    actor text NOT NULL,
    action text NOT NULL,
    details jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT audit_log_pkey PRIMARY KEY (id),
    CONSTRAINT audit_log_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES merchants(id)
);
CREATE INDEX audit_log_merchant ON audit_log USING btree (merchant_id, id);

CREATE TABLE products (
    id text NOT NULL,
    merchant_id text NOT NULL,
    livemode boolean NOT NULL,
    name text NOT NULL,
    price_type text NOT NULL,
    amount_cents base_units,
    min_amount_cents base_units,
    max_amount_cents base_units,
    suggested_amounts_cents base_units[],
    success_url text,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    active boolean DEFAULT true NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    description text,
    last_paid_at timestamp with time zone,
    CONSTRAINT products_amount_cents_check CHECK (((amount_cents)::numeric > (0)::numeric)),
    CONSTRAINT products_description_check CHECK (((length(description) >= 1) AND (length(description) <= 500))),
    CONSTRAINT products_metadata_check CHECK ((jsonb_typeof(metadata) = 'object'::text)),
    CONSTRAINT products_min_amount_cents_check CHECK (((min_amount_cents)::numeric > (0)::numeric)),
    CONSTRAINT products_name_check CHECK (((length(name) >= 1) AND (length(name) <= 200))),
    CONSTRAINT products_price_shape CHECK ((((price_type = 'fixed'::text) AND (amount_cents IS NOT NULL) AND (min_amount_cents IS NULL) AND (max_amount_cents IS NULL) AND (suggested_amounts_cents IS NULL)) OR ((price_type = 'open'::text) AND (amount_cents IS NULL) AND (min_amount_cents IS NOT NULL) AND ((max_amount_cents IS NULL) OR ((max_amount_cents)::numeric >= (min_amount_cents)::numeric)) AND ((suggested_amounts_cents IS NULL) OR ((cardinality(suggested_amounts_cents) >= 1) AND (cardinality(suggested_amounts_cents) <= 3)))))),
    CONSTRAINT products_price_type_check CHECK ((price_type = ANY (ARRAY['fixed'::text, 'open'::text]))),
    CONSTRAINT products_owner UNIQUE (id, merchant_id, livemode),
    CONSTRAINT products_pkey PRIMARY KEY (id),
    CONSTRAINT products_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES merchants(id)
);
CREATE INDEX products_activity ON products USING btree (merchant_id, livemode, active, COALESCE(last_paid_at, '-infinity'::timestamp with time zone), id);
CREATE INDEX products_list ON products USING btree (merchant_id, livemode, id DESC);
CREATE CONSTRAINT TRIGGER products_payout AFTER INSERT ON products DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION payout_check_product();

CREATE TABLE customer_addresses (
    id text NOT NULL,
    merchant_id text NOT NULL,
    livemode boolean NOT NULL,
    customer_id text NOT NULL,
    product_id text NOT NULL,
    salt bytea NOT NULL,
    status text NOT NULL,
    replaced_by text,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone NOT NULL,
    rotated_at timestamp with time zone,
    current_session_id text,
    CONSTRAINT customer_addresses_check CHECK (((status = 'rotated'::text) = (rotated_at IS NOT NULL))),
    CONSTRAINT customer_addresses_check1 CHECK (((status = 'rotated'::text) = (replaced_by IS NOT NULL))),
    CONSTRAINT customer_addresses_check2 CHECK ((replaced_by IS DISTINCT FROM id)),
    CONSTRAINT customer_addresses_customer_id_check CHECK ((customer_id ~ '^[A-Za-z0-9._:-]{1,128}$'::text)),
    CONSTRAINT customer_addresses_metadata_check CHECK ((jsonb_typeof(metadata) = 'object'::text)),
    CONSTRAINT customer_addresses_salt_check CHECK ((length(salt) = 32)),
    CONSTRAINT customer_addresses_status_check CHECK ((status = ANY (ARRAY['active'::text, 'rotated'::text]))),
    CONSTRAINT customer_addresses_id_salt_key UNIQUE (id, salt),
    CONSTRAINT customer_addresses_owner UNIQUE (id, merchant_id, livemode),
    CONSTRAINT customer_addresses_pkey PRIMARY KEY (id),
    CONSTRAINT customer_addresses_salt_key UNIQUE (salt),
    CONSTRAINT customer_addresses_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES merchants(id),
    CONSTRAINT customer_addresses_product_id_fkey FOREIGN KEY (product_id) REFERENCES products(id),
    CONSTRAINT customer_addresses_product_owner FOREIGN KEY (product_id, merchant_id, livemode) REFERENCES products(id, merchant_id, livemode),
    CONSTRAINT customer_addresses_replaced_by_fkey FOREIGN KEY (replaced_by) REFERENCES customer_addresses(id) DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT customer_addresses_replaced_by_owner FOREIGN KEY (replaced_by, merchant_id, livemode) REFERENCES customer_addresses(id, merchant_id, livemode) DEFERRABLE INITIALLY DEFERRED
);
CREATE UNIQUE INDEX customer_addresses_active ON customer_addresses USING btree (merchant_id, livemode, customer_id) WHERE (status = 'active'::text);
CREATE INDEX customer_addresses_customer ON customer_addresses USING btree (merchant_id, livemode, customer_id, id DESC);
CREATE INDEX customer_addresses_merchant ON customer_addresses USING btree (merchant_id, livemode, id DESC);

CREATE TABLE payment_links (
    id text NOT NULL,
    merchant_id text NOT NULL,
    livemode boolean NOT NULL,
    product_id text NOT NULL,
    name text,
    amount_cents base_units,
    active boolean DEFAULT true NOT NULL,
    expires_at timestamp with time zone,
    max_payments integer,
    success_url text,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    last_paid_at timestamp with time zone,
    CONSTRAINT payment_links_amount_cents_check CHECK (((amount_cents)::numeric > (0)::numeric)),
    CONSTRAINT payment_links_expiry CHECK (((expires_at IS NULL) OR (expires_at > created_at))),
    CONSTRAINT payment_links_max_payments_check CHECK (((max_payments >= 1) AND (max_payments <= 1000000))),
    CONSTRAINT payment_links_metadata_check CHECK ((jsonb_typeof(metadata) = 'object'::text)),
    CONSTRAINT payment_links_name_check CHECK (((length(name) >= 1) AND (length(name) <= 200))),
    CONSTRAINT payment_links_id_product_id_key UNIQUE (id, product_id),
    CONSTRAINT payment_links_pkey PRIMARY KEY (id),
    CONSTRAINT payment_links_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES merchants(id),
    CONSTRAINT payment_links_product_id_merchant_id_livemode_fkey FOREIGN KEY (product_id, merchant_id, livemode) REFERENCES products(id, merchant_id, livemode)
);
CREATE INDEX payment_links_list ON payment_links USING btree (merchant_id, livemode, id DESC);
CREATE INDEX payment_links_product ON payment_links USING btree (product_id, id DESC);

CREATE TABLE sessions (
    id text NOT NULL,
    merchant_id text NOT NULL,
    livemode boolean NOT NULL,
    product_id text,
    amount_cents base_units,
    currency text DEFAULT 'USD'::text NOT NULL,
    status text NOT NULL,
    version bigint NOT NULL,
    salt bytea NOT NULL,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    success_url text,
    expires_at timestamp with time zone NOT NULL,
    expired_at timestamp with time zone,
    paid_at timestamp with time zone,
    review_reason text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    client_reference_id text,
    client_reference_source text,
    link_id text,
    checkout_seen_at timestamp with time zone,
    recheck_requested_at timestamp with time zone,
    customer_address_id text,
    pay_window_seconds integer DEFAULT 1800 NOT NULL,
    payment_started_at timestamp with time zone,
    wallet_tx_at timestamp with time zone,
    CONSTRAINT sessions_amount_cents_check CHECK (((amount_cents)::numeric > (0)::numeric)),
    CONSTRAINT sessions_check CHECK ((expires_at > created_at)),
    CONSTRAINT sessions_client_reference_id_check CHECK ((client_reference_id ~ '^[A-Za-z0-9._:-]{1,128}$'::text)),
    CONSTRAINT sessions_client_reference_source_check CHECK ((client_reference_source = ANY (ARRAY['merchant'::text, 'payer'::text]))),
    CONSTRAINT sessions_client_reference_with_source CHECK (((client_reference_id IS NULL) = (client_reference_source IS NULL))),
    CONSTRAINT sessions_currency_check CHECK ((currency = 'USD'::text)),
    CONSTRAINT sessions_metadata_check CHECK ((jsonb_typeof(metadata) = 'object'::text)),
    CONSTRAINT sessions_open_amount CHECK (((amount_cents IS NOT NULL) OR (customer_address_id IS NOT NULL))),
    CONSTRAINT sessions_pay_window_positive CHECK ((pay_window_seconds > 0)),
    CONSTRAINT sessions_salt_check CHECK ((length(salt) = 32)),
    CONSTRAINT sessions_status_check CHECK ((status = ANY (ARRAY['open'::text, 'partially_paid'::text, 'paid'::text, 'overpaid'::text, 'expired'::text, 'late_payment'::text, 'under_review'::text]))),
    CONSTRAINT sessions_version_check CHECK ((version >= 1)),
    CONSTRAINT sessions_id_customer_address UNIQUE (id, customer_address_id),
    CONSTRAINT sessions_owner UNIQUE (id, merchant_id, livemode),
    CONSTRAINT sessions_pkey PRIMARY KEY (id),
    CONSTRAINT sessions_customer_address FOREIGN KEY (customer_address_id, salt) REFERENCES customer_addresses(id, salt),
    CONSTRAINT sessions_customer_address_owner FOREIGN KEY (customer_address_id, merchant_id, livemode) REFERENCES customer_addresses(id, merchant_id, livemode),
    CONSTRAINT sessions_link FOREIGN KEY (link_id, product_id) REFERENCES payment_links(id, product_id),
    CONSTRAINT sessions_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES merchants(id),
    CONSTRAINT sessions_product_id_fkey FOREIGN KEY (product_id) REFERENCES products(id),
    CONSTRAINT sessions_product_owner FOREIGN KEY (product_id, merchant_id, livemode) REFERENCES products(id, merchant_id, livemode)
);
CREATE INDEX sessions_client_reference ON sessions USING btree (merchant_id, livemode, client_reference_id) WHERE (client_reference_id IS NOT NULL);
CREATE INDEX sessions_customer_address ON sessions USING btree (customer_address_id, id DESC) WHERE (customer_address_id IS NOT NULL);
CREATE INDEX sessions_expires_at ON sessions USING btree (expires_at);
CREATE INDEX sessions_expiring ON sessions USING btree (expires_at) WHERE (status = ANY (ARRAY['open'::text, 'partially_paid'::text]));
CREATE INDEX sessions_link ON sessions USING btree (link_id, status) WHERE (link_id IS NOT NULL);
CREATE INDEX sessions_merchant ON sessions USING btree (merchant_id, livemode, id DESC);
CREATE INDEX sessions_merchant_amount ON sessions USING btree (merchant_id, livemode, amount_cents);
CREATE INDEX sessions_merchant_id_pattern ON sessions USING btree (merchant_id, livemode, id text_pattern_ops);
CREATE INDEX sessions_product ON sessions USING btree (product_id, status) WHERE (product_id IS NOT NULL);
CREATE INDEX sessions_recheck_requested ON sessions USING btree (recheck_requested_at) WHERE (recheck_requested_at IS NOT NULL);
CREATE UNIQUE INDEX sessions_salt_key ON sessions USING btree (salt) WHERE (customer_address_id IS NULL);

CREATE TABLE session_pay_to (
    session_id text NOT NULL,
    "position" smallint NOT NULL,
    chain text NOT NULL,
    asset text NOT NULL,
    token text NOT NULL,
    decimals smallint NOT NULL,
    address text NOT NULL,
    amount base_units,
    treasury text NOT NULL,
    fee_bps integer NOT NULL,
    fixed_fee_micros bigint NOT NULL,
    program text,
    rent_receiver text,
    family text NOT NULL,
    bound_tokens text[] NOT NULL,
    CONSTRAINT session_pay_to_amount_check CHECK (((amount)::numeric > (0)::numeric)),
    CONSTRAINT session_pay_to_bound_tokens CHECK (cardinality(bound_tokens) BETWEEN 1 AND 4 AND array_position(bound_tokens, NULL) IS NULL),
    CONSTRAINT session_pay_to_decimals_check CHECK (((decimals >= 6) AND (decimals <= 18))),
    CONSTRAINT session_pay_to_family CHECK ((family = ANY (ARRAY['evm'::text, 'solana'::text]))),
    CONSTRAINT session_pay_to_fee_bps_check CHECK (((fee_bps >= 0) AND (fee_bps <= 300))),
    CONSTRAINT session_pay_to_fixed_fee_micros_check CHECK (((fixed_fee_micros >= 0) AND (fixed_fee_micros <= 5000000))),
    CONSTRAINT session_pay_to_position_check CHECK (("position" >= 0)),
    CONSTRAINT session_pay_to_solana_commitment CHECK (((program IS NULL) = (rent_receiver IS NULL))),
    CONSTRAINT session_pay_to_pkey PRIMARY KEY (session_id, chain, asset),
    CONSTRAINT session_pay_to_session_id_position_key UNIQUE (session_id, "position"),
    CONSTRAINT session_pay_to_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id)
);
CREATE INDEX session_pay_to_address ON session_pay_to USING btree (chain, address);
CREATE CONSTRAINT TRIGGER session_pay_to_payout AFTER INSERT ON session_pay_to DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION payout_check_session();

CREATE TABLE payments (
    id text NOT NULL,
    session_id text NOT NULL,
    chain text NOT NULL,
    asset text NOT NULL,
    address text NOT NULL,
    tx_hash text NOT NULL,
    log_index integer NOT NULL,
    amount base_units NOT NULL,
    decimals smallint NOT NULL,
    status text NOT NULL,
    late boolean NOT NULL,
    detected_at timestamp with time zone NOT NULL,
    confirmed_at timestamp with time zone,
    finalized_at timestamp with time zone,
    reversed_at timestamp with time zone,
    simulated boolean DEFAULT false NOT NULL,
    CONSTRAINT payments_amount_check CHECK (((amount)::numeric > (0)::numeric)),
    CONSTRAINT payments_decimals_check CHECK (((decimals >= 6) AND (decimals <= 18))),
    CONSTRAINT payments_log_index_check CHECK ((log_index >= 0)),
    CONSTRAINT payments_status_check CHECK ((status = ANY (ARRAY['detected'::text, 'confirmed'::text, 'finalized'::text, 'reversed'::text]))),
    CONSTRAINT payments_tx_hash_check CHECK ((tx_hash <> ''::text)),
    CONSTRAINT payments_pkey PRIMARY KEY (id),
    CONSTRAINT payments_session_id_chain_asset_fkey FOREIGN KEY (session_id, chain, asset) REFERENCES session_pay_to(session_id, chain, asset)
);
CREATE UNIQUE INDEX payments_onchain_identity ON payments USING btree (chain, tx_hash, log_index) WHERE (status <> 'reversed'::text);
CREATE INDEX payments_pending ON payments USING btree (chain, status) WHERE (status = ANY (ARRAY['detected'::text, 'confirmed'::text]));
CREATE INDEX payments_session ON payments USING btree (session_id);
CREATE INDEX payments_tx_hash_lower_pattern ON payments USING btree (lower(tx_hash) text_pattern_ops);
CREATE INDEX payments_tx_hash_pattern ON payments USING btree (tx_hash text_pattern_ops);

CREATE TABLE settlements (
    id text NOT NULL,
    session_id text NOT NULL,
    chain text NOT NULL,
    asset text NOT NULL,
    gross base_units NOT NULL,
    fee base_units NOT NULL,
    net base_units NOT NULL,
    tx_hash text NOT NULL,
    log_index integer NOT NULL,
    settled_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    simulated boolean DEFAULT false NOT NULL,
    CONSTRAINT settlements_check CHECK ((((fee)::numeric + (net)::numeric) = (gross)::numeric)),
    CONSTRAINT settlements_gross_check CHECK (((gross)::numeric > (0)::numeric)),
    CONSTRAINT settlements_log_index_check CHECK ((log_index >= 0)),
    CONSTRAINT settlements_net_check CHECK (((net)::numeric > (0)::numeric)),
    CONSTRAINT settlements_tx_hash_check CHECK ((tx_hash <> ''::text)),
    CONSTRAINT settlements_chain_tx_hash_log_index_key UNIQUE (chain, tx_hash, log_index),
    CONSTRAINT settlements_pkey PRIMARY KEY (id),
    CONSTRAINT settlements_session_id_chain_asset_fkey FOREIGN KEY (session_id, chain, asset) REFERENCES session_pay_to(session_id, chain, asset)
);
CREATE INDEX settlements_session ON settlements USING btree (session_id);
CREATE CONSTRAINT TRIGGER settlements_paid_out AFTER INSERT OR UPDATE ON settlements DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION payout_check_settlement();

CREATE TABLE events (
    pk bigserial NOT NULL,
    id text NOT NULL,
    seq bigint,
    merchant_id text NOT NULL,
    livemode boolean NOT NULL,
    type text NOT NULL,
    session_id text,
    session_version bigint,
    payload json NOT NULL,
    created_at timestamp with time zone NOT NULL,
    sequenced_at timestamp with time zone,
    CONSTRAINT events_seq_check CHECK ((seq > 0)),
    CONSTRAINT events_owner UNIQUE (id, merchant_id, livemode),
    CONSTRAINT events_pk_key UNIQUE (pk),
    CONSTRAINT events_pkey PRIMARY KEY (id),
    CONSTRAINT events_seq_key UNIQUE (seq),
    CONSTRAINT events_session_id_session_version_key UNIQUE (session_id, session_version),
    CONSTRAINT events_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES merchants(id),
    CONSTRAINT events_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id)
);
CREATE INDEX events_feed ON events USING btree (merchant_id, livemode, seq) WHERE (seq IS NOT NULL);
CREATE INDEX events_unsequenced ON events USING btree (pk) WHERE (seq IS NULL);

CREATE TABLE webhook_endpoints (
    id text NOT NULL,
    merchant_id text NOT NULL,
    livemode boolean NOT NULL,
    url text NOT NULL,
    enabled_events text[] DEFAULT '{}'::text[] NOT NULL,
    secret_ciphertext bytea NOT NULL,
    previous_secret_ciphertext bytea,
    previous_secret_expires_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    disabled_at timestamp with time zone,
    deleted_at timestamp with time zone,
    description text,
    status text DEFAULT 'enabled'::text NOT NULL,
    disabled_reason text,
    failing_since timestamp with time zone,
    failing_warned_at timestamp with time zone,
    outcome_started_at timestamp with time zone,
    CONSTRAINT webhook_endpoints_check CHECK (((previous_secret_ciphertext IS NULL) = (previous_secret_expires_at IS NULL))),
    CONSTRAINT webhook_endpoints_description_check CHECK (((char_length(description) >= 1) AND (char_length(description) <= 200))),
    CONSTRAINT webhook_endpoints_disabled_reason_check CHECK ((disabled_reason = ANY (ARRAY['merchant'::text, 'failing'::text]))),
    CONSTRAINT webhook_endpoints_disabled_shape CHECK ((((status = 'enabled'::text) = (disabled_reason IS NULL)) AND ((status = 'enabled'::text) = (disabled_at IS NULL)))),
    CONSTRAINT webhook_endpoints_status_check CHECK ((status = ANY (ARRAY['enabled'::text, 'disabled'::text]))),
    CONSTRAINT webhook_endpoints_url_check CHECK ((url <> ''::text)),
    CONSTRAINT webhook_endpoints_warned_in_streak CHECK (((failing_warned_at IS NULL) OR (failing_since IS NOT NULL))),
    CONSTRAINT webhook_endpoints_owner UNIQUE (id, merchant_id, livemode),
    CONSTRAINT webhook_endpoints_pkey PRIMARY KEY (id),
    CONSTRAINT webhook_endpoints_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES merchants(id)
);
CREATE INDEX webhook_endpoints_merchant ON webhook_endpoints USING btree (merchant_id, livemode);

CREATE TABLE webhook_deliveries (
    id text NOT NULL,
    endpoint_id text NOT NULL,
    event_id text,
    status text NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    next_attempt_at timestamp with time zone NOT NULL,
    last_attempt_at timestamp with time zone,
    last_status_code integer,
    last_error text,
    delivered_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    test boolean DEFAULT false NOT NULL,
    test_event_id text,
    test_event_type text,
    merchant_id text NOT NULL,
    livemode boolean NOT NULL,
    CONSTRAINT webhook_deliveries_attempts_check CHECK ((attempts >= 0)),
    CONSTRAINT webhook_deliveries_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'succeeded'::text, 'dead'::text]))),
    CONSTRAINT webhook_deliveries_test_shape CHECK (((test AND (event_id IS NULL) AND (test_event_id IS NOT NULL) AND (test_event_type IS NOT NULL) AND (status <> 'pending'::text)) OR ((NOT test) AND (event_id IS NOT NULL) AND (test_event_id IS NULL) AND (test_event_type IS NULL)))),
    CONSTRAINT webhook_deliveries_endpoint_id_event_id_key UNIQUE (endpoint_id, event_id),
    CONSTRAINT webhook_deliveries_pkey PRIMARY KEY (id),
    CONSTRAINT webhook_deliveries_endpoint_id_fkey FOREIGN KEY (endpoint_id) REFERENCES webhook_endpoints(id),
    CONSTRAINT webhook_deliveries_endpoint_owner FOREIGN KEY (endpoint_id, merchant_id, livemode) REFERENCES webhook_endpoints(id, merchant_id, livemode),
    CONSTRAINT webhook_deliveries_event_id_fkey FOREIGN KEY (event_id) REFERENCES events(id),
    CONSTRAINT webhook_deliveries_event_owner FOREIGN KEY (event_id, merchant_id, livemode) REFERENCES events(id, merchant_id, livemode)
);
CREATE INDEX webhook_deliveries_due ON webhook_deliveries USING btree (next_attempt_at) WHERE (status = 'pending'::text);

CREATE TABLE idempotency_keys (
    merchant_id text NOT NULL,
    livemode boolean NOT NULL,
    key text NOT NULL,
    request_hash bytea NOT NULL,
    response_status integer NOT NULL,
    response_body bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT idempotency_keys_key_check CHECK (((length(key) >= 1) AND (length(key) <= 255))),
    CONSTRAINT idempotency_keys_request_hash_check CHECK ((length(request_hash) = 32)),
    CONSTRAINT idempotency_keys_pkey PRIMARY KEY (merchant_id, livemode, key),
    CONSTRAINT idempotency_keys_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES merchants(id)
);
CREATE INDEX idempotency_keys_created ON idempotency_keys USING btree (created_at);

CREATE TABLE chain_checkpoints (
    chain text NOT NULL,
    block_number bigint NOT NULL,
    block_hash text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chain_checkpoints_block_hash_check CHECK ((block_hash ~ '^0x[0-9a-f]{64}$'::text)),
    CONSTRAINT chain_checkpoints_block_number_check CHECK ((block_number >= 0)),
    CONSTRAINT chain_checkpoints_pkey PRIMARY KEY (chain, block_number)
);

CREATE TABLE chain_watch (
    chain text NOT NULL,
    address text NOT NULL,
    session_id text NOT NULL,
    first_block bigint NOT NULL,
    swept_at timestamp with time zone,
    swept_balance base_units,
    next_check_at timestamp with time zone,
    checked_block bigint,
    last_balance base_units,
    checked_at timestamp with time zone,
    rechecked_for timestamp with time zone,
    token_balances jsonb,
    forwarder_seen_at timestamp with time zone,
    token_blocks jsonb,
    CONSTRAINT chain_watch_address_check CHECK ((address ~ '^0x[0-9a-f]{40}$'::text)),
    CONSTRAINT chain_watch_checked_block_check CHECK ((checked_block >= 0)),
    CONSTRAINT chain_watch_first_block_check CHECK ((first_block >= 0)),
    CONSTRAINT chain_watch_pkey PRIMARY KEY (chain, address),
    CONSTRAINT chain_watch_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id)
);
CREATE INDEX chain_watch_next_check ON chain_watch USING btree (chain, next_check_at);

CREATE TABLE chain_payments (
    payment_id text NOT NULL,
    session_id text NOT NULL,
    chain text NOT NULL,
    asset text NOT NULL,
    kind text NOT NULL,
    block_number bigint NOT NULL,
    block_hash text NOT NULL,
    split_log_index integer,
    split_fee base_units,
    split_net base_units,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    split_payouts base_units[],
    finality_check_at timestamp with time zone,
    CONSTRAINT chain_payments_block_hash_check CHECK (((block_hash ~ '^0x[0-9a-f]{64}$'::text) OR (block_hash ~ '^[1-9A-HJ-NP-Za-km-z]{32,44}$'::text))),
    CONSTRAINT chain_payments_block_number_check CHECK ((block_number >= 0)),
    CONSTRAINT chain_payments_check CHECK (((kind = 'gateway'::text) = ((split_log_index IS NOT NULL) AND (split_fee IS NOT NULL) AND (split_net IS NOT NULL)))),
    CONSTRAINT chain_payments_kind_check CHECK ((kind = ANY (ARRAY['transfer'::text, 'gateway'::text]))),
    CONSTRAINT chain_payments_split_log_index_check CHECK ((split_log_index >= 0)),
    CONSTRAINT chain_payments_split_payouts CHECK ((((kind = 'gateway'::text) = (split_payouts IS NOT NULL)) AND ((split_payouts IS NULL) OR ((cardinality(split_payouts) >= 1) AND (cardinality(split_payouts) <= 5))))),
    CONSTRAINT chain_payments_pkey PRIMARY KEY (payment_id),
    CONSTRAINT chain_payments_payment_id_fkey FOREIGN KEY (payment_id) REFERENCES payments(id),
    CONSTRAINT chain_payments_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id)
);
CREATE INDEX chain_payments_block ON chain_payments USING btree (chain, block_number);
CREATE INDEX chain_payments_session ON chain_payments USING btree (session_id);

CREATE TABLE chain_tx_jobs (
    id text NOT NULL,
    chain text NOT NULL,
    kind text NOT NULL,
    ref text NOT NULL,
    session_id text NOT NULL,
    asset text NOT NULL,
    address text,
    planned_total base_units,
    to_address text NOT NULL,
    calldata bytea NOT NULL,
    sender text NOT NULL,
    details jsonb DEFAULT '{}'::jsonb NOT NULL,
    status text NOT NULL,
    outcome text,
    attempts integer DEFAULT 0 NOT NULL,
    next_attempt_at timestamp with time zone NOT NULL,
    nonce bigint,
    gas_limit bigint,
    max_fee base_units,
    tip base_units,
    tx_hashes text[] DEFAULT '{}'::text[] NOT NULL,
    raw_tx bytea,
    submitted_at timestamp with time zone,
    mined_tx_hash text,
    mined_block bigint,
    mined_block_hash text,
    gas_used bigint,
    last_error text,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    last_valid_height bigint,
    finality_check_at timestamp with time zone,
    CONSTRAINT chain_tx_jobs_attempts_check CHECK ((attempts >= 0)),
    CONSTRAINT chain_tx_jobs_check CHECK (((kind <> 'flush'::text) OR ((address IS NOT NULL) AND (planned_total IS NOT NULL)))),
    CONSTRAINT chain_tx_jobs_check1 CHECK (((status <> ALL (ARRAY['submitted'::text, 'mined'::text])) OR (nonce IS NOT NULL) OR (last_valid_height IS NOT NULL))),
    CONSTRAINT chain_tx_jobs_kind_check CHECK ((kind = ANY (ARRAY['flush'::text, 'settle'::text]))),
    CONSTRAINT chain_tx_jobs_last_valid_height_check CHECK ((last_valid_height >= 0)),
    CONSTRAINT chain_tx_jobs_nonce_check CHECK ((nonce >= 0)),
    CONSTRAINT chain_tx_jobs_ref_check CHECK ((ref <> ''::text)),
    CONSTRAINT chain_tx_jobs_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'submitted'::text, 'mined'::text, 'done'::text, 'failed'::text, 'blocked'::text]))),
    CONSTRAINT chain_tx_jobs_chain_kind_ref_key UNIQUE (chain, kind, ref),
    CONSTRAINT chain_tx_jobs_pkey PRIMARY KEY (id),
    CONSTRAINT chain_tx_jobs_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id)
);
CREATE INDEX chain_tx_jobs_open ON chain_tx_jobs USING btree (chain, status, next_attempt_at) WHERE (status = ANY (ARRAY['pending'::text, 'submitted'::text, 'mined'::text, 'blocked'::text]));
CREATE INDEX chain_tx_jobs_session ON chain_tx_jobs USING btree (session_id);

CREATE TABLE chain_status (
    chain text NOT NULL,
    state text NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    since timestamp with time zone NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chain_status_state_check CHECK ((state = ANY (ARRAY['ok'::text, 'degraded'::text, 'halted'::text]))),
    CONSTRAINT chain_status_pkey PRIMARY KEY (chain)
);

CREATE TABLE solana_watch (
    chain text NOT NULL,
    address text NOT NULL,
    kind text NOT NULL,
    session_id text NOT NULL,
    asset text,
    cursor_signature text,
    cursor_slot bigint,
    swept_at timestamp with time zone,
    swept_balance base_units,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    next_check_at timestamp with time zone,
    last_balance base_units,
    checked_at timestamp with time zone,
    rechecked_for timestamp with time zone,
    balance_slot bigint,
    CONSTRAINT solana_watch_address_check CHECK ((address ~ '^[1-9A-HJ-NP-Za-km-z]{32,44}$'::text)),
    CONSTRAINT solana_watch_check CHECK (((cursor_signature IS NULL) = (cursor_slot IS NULL))),
    CONSTRAINT solana_watch_check1 CHECK (((kind = 'deposit'::text) = (asset IS NOT NULL))),
    CONSTRAINT solana_watch_cursor_slot_check CHECK ((cursor_slot >= 0)),
    CONSTRAINT solana_watch_kind_check CHECK ((kind = ANY (ARRAY['deposit'::text, 'reference'::text]))),
    CONSTRAINT solana_watch_pkey PRIMARY KEY (chain, address),
    CONSTRAINT solana_watch_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id)
);
CREATE INDEX solana_watch_next_check ON solana_watch USING btree (chain, next_check_at);
CREATE INDEX solana_watch_session ON solana_watch USING btree (session_id);

CREATE UNLOGGED TABLE rate_limits (
    key text NOT NULL,
    tokens bigint NOT NULL,
    allowed boolean NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT rate_limits_key_check CHECK (((key <> ''::text) AND (length(key) <= 200))),
    CONSTRAINT rate_limits_tokens_check CHECK ((tokens >= 0)),
    CONSTRAINT rate_limits_pkey PRIMARY KEY (key)
);
CREATE INDEX rate_limits_updated ON rate_limits USING btree (updated_at);

CREATE TABLE dashboard_users (
    id text NOT NULL,
    merchant_id text NOT NULL,
    email text NOT NULL,
    password_hash text NOT NULL,
    totp_secret_ciphertext bytea,
    totp_pending_ciphertext bytea,
    totp_last_step bigint,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    disabled_at timestamp with time zone,
    totp_pending_session bytea,
    totp_pending_expires_at timestamp with time zone,
    email_verified_at timestamp with time zone,
    code_failures integer DEFAULT 0 NOT NULL,
    code_window_start timestamp with time zone,
    totp_failures integer DEFAULT 0 NOT NULL,
    totp_retry_at timestamp with time zone,
    CONSTRAINT dashboard_users_code_failures_check CHECK ((code_failures >= 0)),
    CONSTRAINT dashboard_users_email_check CHECK (((email = lower(email)) AND ((length(email) >= 3) AND (length(email) <= 254)) AND (POSITION(('@'::text) IN (email)) > 1))),
    CONSTRAINT dashboard_users_password_hash_check CHECK ((password_hash ~~ '$argon2id$%'::text)),
    CONSTRAINT dashboard_users_totp_failures_check CHECK ((totp_failures >= 0)),
    CONSTRAINT dashboard_users_totp_pending CHECK ((((totp_pending_ciphertext IS NULL) AND (totp_pending_session IS NULL) AND (totp_pending_expires_at IS NULL)) OR ((totp_pending_ciphertext IS NOT NULL) AND (totp_pending_session IS NOT NULL) AND (totp_pending_expires_at IS NOT NULL)))),
    CONSTRAINT dashboard_users_totp_pending_session_check CHECK ((length(totp_pending_session) = 32)),
    CONSTRAINT dashboard_users_pkey PRIMARY KEY (id),
    CONSTRAINT dashboard_users_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES merchants(id)
);
CREATE UNIQUE INDEX dashboard_users_email ON dashboard_users USING btree (email);
CREATE INDEX dashboard_users_merchant ON dashboard_users USING btree (merchant_id);

CREATE TABLE dashboard_sessions (
    token_hash bytea NOT NULL,
    user_id text NOT NULL,
    csrf_token text NOT NULL,
    created_at timestamp with time zone NOT NULL,
    last_seen_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    CONSTRAINT dashboard_sessions_check CHECK ((expires_at > created_at)),
    CONSTRAINT dashboard_sessions_check1 CHECK ((last_seen_at >= created_at)),
    CONSTRAINT dashboard_sessions_csrf_token_check CHECK ((length(csrf_token) >= 32)),
    CONSTRAINT dashboard_sessions_token_hash_check CHECK ((length(token_hash) = 32)),
    CONSTRAINT dashboard_sessions_pkey PRIMARY KEY (token_hash),
    CONSTRAINT dashboard_sessions_user_id_fkey FOREIGN KEY (user_id) REFERENCES dashboard_users(id) ON DELETE CASCADE
);
CREATE INDEX dashboard_sessions_expires ON dashboard_sessions USING btree (expires_at);
CREATE INDEX dashboard_sessions_user ON dashboard_sessions USING btree (user_id);

CREATE TABLE sandbox_inboxes (
    id text NOT NULL,
    merchant_id text NOT NULL,
    token text NOT NULL,
    endpoint_id text NOT NULL,
    created_at timestamp with time zone NOT NULL,
    disabled_at timestamp with time zone,
    CONSTRAINT sandbox_inboxes_token_check CHECK ((length(token) >= 40)),
    CONSTRAINT sandbox_inboxes_endpoint_id_key UNIQUE (endpoint_id),
    CONSTRAINT sandbox_inboxes_pkey PRIMARY KEY (id),
    CONSTRAINT sandbox_inboxes_token_key UNIQUE (token),
    CONSTRAINT sandbox_inboxes_endpoint_id_fkey FOREIGN KEY (endpoint_id) REFERENCES webhook_endpoints(id),
    CONSTRAINT sandbox_inboxes_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES merchants(id)
);
CREATE UNIQUE INDEX sandbox_inboxes_active ON sandbox_inboxes USING btree (merchant_id) WHERE (disabled_at IS NULL);

CREATE TABLE sandbox_requests (
    seq bigserial NOT NULL,
    id text NOT NULL,
    inbox_id text NOT NULL,
    received_at timestamp with time zone NOT NULL,
    method text NOT NULL,
    headers jsonb NOT NULL,
    body bytea NOT NULL,
    signature_valid boolean NOT NULL,
    signature_error text,
    signature_timestamp timestamp with time zone,
    CONSTRAINT sandbox_requests_check CHECK ((signature_valid = (signature_error IS NULL))),
    CONSTRAINT sandbox_requests_headers_check CHECK ((jsonb_typeof(headers) = 'array'::text)),
    CONSTRAINT sandbox_requests_signature_error_check CHECK ((signature_error = ANY (ARRAY['missing'::text, 'malformed'::text, 'no_match'::text, 'timestamp_outside_tolerance'::text]))),
    CONSTRAINT sandbox_requests_pkey PRIMARY KEY (id),
    CONSTRAINT sandbox_requests_seq_key UNIQUE (seq),
    CONSTRAINT sandbox_requests_inbox_id_fkey FOREIGN KEY (inbox_id) REFERENCES sandbox_inboxes(id) ON DELETE CASCADE
);
CREATE INDEX sandbox_requests_inbox ON sandbox_requests USING btree (inbox_id, seq);
CREATE INDEX sandbox_requests_received ON sandbox_requests USING btree (received_at);

CREATE TABLE wallets (
    id text NOT NULL,
    merchant_id text NOT NULL,
    livemode boolean NOT NULL,
    family text NOT NULL,
    label text NOT NULL,
    address text NOT NULL,
    verified boolean NOT NULL,
    created_at timestamp with time zone NOT NULL,
    archived_at timestamp with time zone,
    seq bigint GENERATED ALWAYS AS IDENTITY,
    CONSTRAINT wallets_address_check CHECK ((address <> ''::text)),
    CONSTRAINT wallets_check CHECK (((archived_at IS NULL) OR (archived_at >= created_at))),
    CONSTRAINT wallets_family_check CHECK ((family = ANY (ARRAY['evm'::text, 'solana'::text]))),
    CONSTRAINT wallets_label_check CHECK (((length(label) >= 1) AND (length(label) <= 100))),
    CONSTRAINT wallets_id_merchant_id_livemode_family_key UNIQUE (id, merchant_id, livemode, family),
    CONSTRAINT wallets_pkey PRIMARY KEY (id),
    CONSTRAINT wallets_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES merchants(id)
);
CREATE UNIQUE INDEX wallets_active_address ON wallets USING btree (merchant_id, livemode, family, address) WHERE (archived_at IS NULL);
CREATE INDEX wallets_merchant ON wallets USING btree (merchant_id, livemode, family, created_at);
CREATE TRIGGER wallets_guard BEFORE UPDATE ON wallets FOR EACH ROW EXECUTE FUNCTION payout_guard_archive();

CREATE TABLE session_payouts (
    session_id text NOT NULL,
    chain_id text NOT NULL,
    family text NOT NULL,
    "position" smallint NOT NULL,
    wallet_id text,
    address text NOT NULL,
    bps integer NOT NULL,
    merchant_id text NOT NULL,
    livemode boolean NOT NULL,
    CONSTRAINT session_payouts_address_check CHECK ((address <> ''::text)),
    CONSTRAINT session_payouts_bps_check CHECK (((bps >= 1) AND (bps <= 10000))),
    CONSTRAINT session_payouts_chain_id_check CHECK ((chain_id <> ''::text)),
    CONSTRAINT session_payouts_family_check CHECK ((family = ANY (ARRAY['evm'::text, 'solana'::text]))),
    CONSTRAINT session_payouts_position_check CHECK ((("position" >= 0) AND ("position" <= 4))),
    CONSTRAINT session_payouts_pkey PRIMARY KEY (session_id, chain_id, "position"),
    CONSTRAINT session_payouts_session_id_chain_id_address_key UNIQUE (session_id, chain_id, address),
    CONSTRAINT session_payouts_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id),
    CONSTRAINT session_payouts_session_owner FOREIGN KEY (session_id, merchant_id, livemode) REFERENCES sessions(id, merchant_id, livemode),
    CONSTRAINT session_payouts_wallet_id_fkey FOREIGN KEY (wallet_id) REFERENCES wallets(id),
    CONSTRAINT session_payouts_wallet_owner FOREIGN KEY (wallet_id, merchant_id, livemode, family) REFERENCES wallets(id, merchant_id, livemode, family)
);
CREATE INDEX session_payouts_wallet ON session_payouts USING btree (wallet_id);
CREATE CONSTRAINT TRIGGER session_payouts_whole AFTER INSERT OR DELETE OR UPDATE ON session_payouts DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION payout_check_session();

CREATE TABLE settlement_payouts (
    settlement_id text NOT NULL,
    "position" smallint NOT NULL,
    wallet_id text,
    address text NOT NULL,
    amount base_units NOT NULL,
    CONSTRAINT settlement_payouts_address_check CHECK ((address <> ''::text)),
    CONSTRAINT settlement_payouts_amount_check CHECK (((amount)::numeric > (0)::numeric)),
    CONSTRAINT settlement_payouts_position_check CHECK ((("position" >= 0) AND ("position" <= 4))),
    CONSTRAINT settlement_payouts_pkey PRIMARY KEY (settlement_id, "position"),
    CONSTRAINT settlement_payouts_settlement_id_fkey FOREIGN KEY (settlement_id) REFERENCES settlements(id),
    CONSTRAINT settlement_payouts_wallet_id_fkey FOREIGN KEY (wallet_id) REFERENCES wallets(id)
);
CREATE INDEX settlement_payouts_wallet ON settlement_payouts USING btree (wallet_id);
CREATE CONSTRAINT TRIGGER settlement_payouts_whole AFTER INSERT OR DELETE OR UPDATE ON settlement_payouts DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION payout_check_settlement();

CREATE TABLE dashboard_email_codes (
    user_id text NOT NULL,
    purpose text NOT NULL,
    code_hash bytea NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    CONSTRAINT dashboard_email_codes_attempts_check CHECK (((attempts >= 0) AND (attempts <= 5))),
    CONSTRAINT dashboard_email_codes_check CHECK (((expires_at > created_at) AND (expires_at <= (created_at + '01:00:00'::interval)))),
    CONSTRAINT dashboard_email_codes_code_hash_check CHECK ((length(code_hash) = 32)),
    CONSTRAINT dashboard_email_codes_purpose_check CHECK ((purpose = ANY (ARRAY['verify_email'::text, 'password_reset'::text]))),
    CONSTRAINT dashboard_email_codes_pkey PRIMARY KEY (user_id, purpose),
    CONSTRAINT dashboard_email_codes_user_id_fkey FOREIGN KEY (user_id) REFERENCES dashboard_users(id) ON DELETE CASCADE
);
CREATE INDEX dashboard_email_codes_expires ON dashboard_email_codes USING btree (expires_at);
CREATE UNIQUE INDEX dashboard_email_codes_reset_link ON dashboard_email_codes USING btree (code_hash) WHERE (purpose = 'password_reset'::text);

CREATE TABLE product_payout_items (
    product_id text NOT NULL,
    merchant_id text NOT NULL,
    livemode boolean NOT NULL,
    chain_id text NOT NULL,
    family text NOT NULL,
    "position" smallint NOT NULL,
    wallet_id text NOT NULL,
    bps integer NOT NULL,
    CONSTRAINT product_payout_items_bps_check CHECK (((bps >= 1) AND (bps <= 10000))),
    CONSTRAINT product_payout_items_chain_id_check CHECK ((chain_id <> ''::text)),
    CONSTRAINT product_payout_items_family_check CHECK ((family = ANY (ARRAY['evm'::text, 'solana'::text]))),
    CONSTRAINT product_payout_items_position_check CHECK ((("position" >= 0) AND ("position" <= 4))),
    CONSTRAINT product_payout_items_pkey PRIMARY KEY (product_id, chain_id, "position"),
    CONSTRAINT product_payout_items_product_id_chain_id_wallet_id_key UNIQUE (product_id, chain_id, wallet_id),
    CONSTRAINT product_payout_items_product_id_merchant_id_livemode_fkey FOREIGN KEY (product_id, merchant_id, livemode) REFERENCES products(id, merchant_id, livemode),
    CONSTRAINT product_payout_items_wallet_id_merchant_id_livemode_family_fkey FOREIGN KEY (wallet_id, merchant_id, livemode, family) REFERENCES wallets(id, merchant_id, livemode, family)
);
CREATE INDEX product_payout_items_wallet ON product_payout_items USING btree (wallet_id);
CREATE CONSTRAINT TRIGGER product_payout_items_whole AFTER INSERT OR DELETE OR UPDATE ON product_payout_items DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION payout_check_product();

CREATE TABLE solana_wallet_txs (
    session_id text NOT NULL,
    chain text NOT NULL,
    asset text NOT NULL,
    payer text NOT NULL,
    gross base_units NOT NULL,
    transaction text NOT NULL,
    last_valid_height bigint NOT NULL,
    issued_at timestamp with time zone NOT NULL,
    CONSTRAINT solana_wallet_txs_gross_check CHECK (((gross)::numeric > (0)::numeric)),
    CONSTRAINT solana_wallet_txs_last_valid_height_check CHECK ((last_valid_height >= 0)),
    CONSTRAINT solana_wallet_txs_pkey PRIMARY KEY (session_id),
    CONSTRAINT solana_wallet_txs_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);

CREATE TABLE chain_scan_state (
    chain text NOT NULL,
    dormant_since timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chain_scan_state_pkey PRIMARY KEY (chain)
);

CREATE TABLE webhook_endpoint_notices (
    id bigserial NOT NULL,
    endpoint_id text NOT NULL,
    kind text NOT NULL,
    failing_since timestamp with time zone NOT NULL,
    last_status_code integer,
    last_error text,
    created_at timestamp with time zone NOT NULL,
    leased_until timestamp with time zone NOT NULL,
    CONSTRAINT webhook_endpoint_notices_kind_check CHECK ((kind = ANY (ARRAY['failing_warning'::text, 'disabled'::text]))),
    CONSTRAINT webhook_endpoint_notices_pkey PRIMARY KEY (id),
    CONSTRAINT webhook_endpoint_notices_endpoint_id_fkey FOREIGN KEY (endpoint_id) REFERENCES webhook_endpoints(id)
);
CREATE INDEX webhook_endpoint_notices_due ON webhook_endpoint_notices USING btree (leased_until);
CREATE UNIQUE INDEX webhook_endpoint_notices_one_warning ON webhook_endpoint_notices USING btree (endpoint_id) WHERE (kind = 'failing_warning'::text);

CREATE TABLE customer_address_pay_to (
    customer_address_id text NOT NULL,
    "position" smallint NOT NULL,
    chain text NOT NULL,
    asset text NOT NULL,
    family text NOT NULL,
    token text NOT NULL,
    decimals smallint NOT NULL,
    address text NOT NULL,
    treasury text NOT NULL,
    fee_bps integer NOT NULL,
    fixed_fee_micros bigint NOT NULL,
    program text,
    rent_receiver text,
    bound_tokens text[] NOT NULL,
    CONSTRAINT customer_address_pay_to_bound_tokens_check CHECK (cardinality(bound_tokens) BETWEEN 1 AND 4 AND array_position(bound_tokens, NULL) IS NULL),
    CONSTRAINT customer_address_pay_to_check CHECK (((program IS NULL) = (rent_receiver IS NULL))),
    CONSTRAINT customer_address_pay_to_decimals_check CHECK (((decimals >= 6) AND (decimals <= 18))),
    CONSTRAINT customer_address_pay_to_family_check CHECK ((family = ANY (ARRAY['evm'::text, 'solana'::text]))),
    CONSTRAINT customer_address_pay_to_fee_bps_check CHECK (((fee_bps >= 0) AND (fee_bps <= 300))),
    CONSTRAINT customer_address_pay_to_fixed_fee_micros_check CHECK (((fixed_fee_micros >= 0) AND (fixed_fee_micros <= 5000000))),
    CONSTRAINT customer_address_pay_to_position_check CHECK (("position" >= 0)),
    CONSTRAINT customer_address_pay_to_customer_address_id_position_key UNIQUE (customer_address_id, "position"),
    CONSTRAINT customer_address_pay_to_pkey PRIMARY KEY (customer_address_id, chain, asset),
    CONSTRAINT customer_address_pay_to_customer_address_id_fkey FOREIGN KEY (customer_address_id) REFERENCES customer_addresses(id)
);
CREATE INDEX customer_address_pay_to_address ON customer_address_pay_to USING btree (chain, address);
CREATE CONSTRAINT TRIGGER customer_address_pay_to_payout AFTER INSERT ON customer_address_pay_to DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION payout_check_customer_address();

CREATE TABLE customer_address_payouts (
    customer_address_id text NOT NULL,
    chain_id text NOT NULL,
    family text NOT NULL,
    "position" smallint NOT NULL,
    wallet_id text,
    address text NOT NULL,
    bps integer NOT NULL,
    merchant_id text NOT NULL,
    livemode boolean NOT NULL,
    CONSTRAINT customer_address_payouts_address_check CHECK ((address <> ''::text)),
    CONSTRAINT customer_address_payouts_bps_check CHECK (((bps >= 1) AND (bps <= 10000))),
    CONSTRAINT customer_address_payouts_chain_id_check CHECK ((chain_id <> ''::text)),
    CONSTRAINT customer_address_payouts_family_check CHECK ((family = ANY (ARRAY['evm'::text, 'solana'::text]))),
    CONSTRAINT customer_address_payouts_position_check CHECK ((("position" >= 0) AND ("position" <= 4))),
    CONSTRAINT customer_address_payouts_customer_address_id_chain_id_addre_key UNIQUE (customer_address_id, chain_id, address),
    CONSTRAINT customer_address_payouts_pkey PRIMARY KEY (customer_address_id, chain_id, "position"),
    CONSTRAINT customer_address_payouts_address_owner FOREIGN KEY (customer_address_id, merchant_id, livemode) REFERENCES customer_addresses(id, merchant_id, livemode),
    CONSTRAINT customer_address_payouts_customer_address_id_fkey FOREIGN KEY (customer_address_id) REFERENCES customer_addresses(id),
    CONSTRAINT customer_address_payouts_wallet_id_fkey FOREIGN KEY (wallet_id) REFERENCES wallets(id),
    CONSTRAINT customer_address_payouts_wallet_owner FOREIGN KEY (wallet_id, merchant_id, livemode, family) REFERENCES wallets(id, merchant_id, livemode, family)
);
CREATE INDEX customer_address_payouts_wallet ON customer_address_payouts USING btree (wallet_id);
CREATE CONSTRAINT TRIGGER customer_address_payouts_whole AFTER INSERT OR DELETE OR UPDATE ON customer_address_payouts DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION payout_check_customer_address();

CREATE TABLE customer_address_cursors (
    customer_address_id text NOT NULL,
    chain text NOT NULL,
    block_number bigint,
    signature text,
    slot bigint,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT customer_address_cursors_block_number_check CHECK ((block_number >= 0)),
    CONSTRAINT customer_address_cursors_check CHECK (((block_number IS NULL) <> (signature IS NULL))),
    CONSTRAINT customer_address_cursors_check1 CHECK (((signature IS NULL) = (slot IS NULL))),
    CONSTRAINT customer_address_cursors_slot_check CHECK ((slot >= 0)),
    CONSTRAINT customer_address_cursors_pkey PRIMARY KEY (customer_address_id, chain),
    CONSTRAINT customer_address_cursors_customer_address_id_fkey FOREIGN KEY (customer_address_id) REFERENCES customer_addresses(id)
);

CREATE TABLE session_reveals (
    session_id text NOT NULL,
    chain text NOT NULL,
    revealed_at timestamp with time zone NOT NULL,
    source text NOT NULL,
    CONSTRAINT session_reveals_source_check CHECK ((source = ANY (ARRAY['checkout'::text, 'merchant_api'::text, 'wallet'::text, 'customer_address'::text]))),
    CONSTRAINT session_reveals_pkey PRIMARY KEY (session_id, chain),
    CONSTRAINT session_reveals_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id)
);
CREATE INDEX session_reveals_chain_revealed_at ON session_reveals USING btree (chain, revealed_at);

CREATE TABLE chain_credit_heads (
    chain text NOT NULL,
    block_number bigint NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chain_credit_heads_block_number_check CHECK ((block_number >= 0)),
    CONSTRAINT chain_credit_heads_pkey PRIMARY KEY (chain)
);

-- Foreign keys that close a cycle between the tables above.
ALTER TABLE customer_addresses
    ADD CONSTRAINT customer_addresses_current_session FOREIGN KEY (current_session_id, id) REFERENCES sessions(id, customer_address_id);

-- +goose Down
ALTER TABLE customer_addresses DROP CONSTRAINT customer_addresses_current_session;
DROP TABLE chain_credit_heads;
DROP TABLE session_reveals;
DROP TABLE customer_address_cursors;
DROP TABLE customer_address_payouts;
DROP TABLE customer_address_pay_to;
DROP TABLE webhook_endpoint_notices;
DROP TABLE chain_scan_state;
DROP TABLE solana_wallet_txs;
DROP TABLE product_payout_items;
DROP TABLE dashboard_email_codes;
DROP TABLE settlement_payouts;
DROP TABLE session_payouts;
DROP TABLE wallets;
DROP TABLE sandbox_requests;
DROP TABLE sandbox_inboxes;
DROP TABLE dashboard_sessions;
DROP TABLE dashboard_users;
DROP TABLE rate_limits;
DROP TABLE solana_watch;
DROP TABLE chain_status;
DROP TABLE chain_tx_jobs;
DROP TABLE chain_payments;
DROP TABLE chain_watch;
DROP TABLE chain_checkpoints;
DROP TABLE idempotency_keys;
DROP TABLE webhook_deliveries;
DROP TABLE webhook_endpoints;
DROP TABLE events;
DROP TABLE settlements;
DROP TABLE payments;
DROP TABLE session_pay_to;
DROP TABLE sessions;
DROP TABLE payment_links;
DROP TABLE customer_addresses;
DROP TABLE products;
DROP TABLE audit_log;
DROP TABLE merchant_assets;
DROP TABLE api_keys;
DROP TABLE merchants;
DROP TABLE logos;
DROP FUNCTION payout_guard_archive();
DROP FUNCTION payout_check_settlement();
DROP FUNCTION payout_check_session();
DROP FUNCTION payout_check_product();
DROP FUNCTION payout_check_customer_address();
DROP DOMAIN base_units;
