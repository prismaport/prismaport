import { useEffect, useMemo, useState } from 'react';
import { Link as RouterLink, useNavigate, useSearchParams } from 'react-router-dom';
import { Icon } from '../components/Icon';
import { Amounts, ErrorState, Loading, PageHead, Pill, Segmented } from '../components/ui';
import type { ProductStatusFilter } from '../lib/endpoints';
import { formatDateTime, formatRelative, productPrice } from '../lib/format';
import { useMode } from '../lib/mode';
import { useProducts } from '../lib/queries';
import { productPill } from '../lib/status';
import type { Product } from '../lib/types';
import { useLinkCreatedToast } from './LinkActions';
import { LinkDrawer } from './LinkDrawer';
import { DeactivateProductDialog, useProductSavedToast, useSetProductActive } from './ProductActions';
import { ProductDrawer } from './ProductDrawer';
import { PayoutNetworks } from './ProductPayout';
import { PayWith } from './ProductNetworks';

/** "2 live of 3", or "No links yet". */
function linkCount(p: Product): string {
  if (p.links.total === 0) return 'No links yet';
  return `${p.links.live.toLocaleString('en-US')} live of ${p.links.total.toLocaleString('en-US')}`;
}

function ProductRow({ product, onCreateLink, onEdit, onDeactivate }: { product: Product; onCreateLink: () => void; onEdit: () => void; onDeactivate: () => void }) {
  const setActive = useSetProductActive();
  return (
    <li className="data-row link-row product-row" data-testid="product-row" data-product-id={product.id}>
      <div className="cell c-name">
        <RouterLink to={`/products/${product.id}`} className="row-link">
          {product.name}
        </RouterLink>
        <div className="meta">
          <span className="amount small" data-testid="product-price">
            {productPrice(product)}
          </span>
        </div>
      </div>
      <div className="cell c-pay">
        <span className="cell-label">Payers can pay with</span>
        {product.payment_methods ? <PayWith product={product} /> : <PayoutNetworks payout={product.payout} />}
      </div>
      <div className="cell c-status">
        <span className="cell-label">Status</span>
        <Pill pill={productPill(product)} />
      </div>
      <div className="c-stats">
        <div className="cell">
          <span className="cell-label">Links</span>
          <span className="small" data-testid="product-links">
            {linkCount(product)}
          </span>
        </div>
        <div className="cell c-received">
          <span className="cell-label">Received</span>
          <Amounts items={product.stats.received} />
        </div>
        <div className="cell">
          <span className="cell-label">Last payment</span>
          <span className="small" title={formatDateTime(product.stats.last_paid_at)}>
            {formatRelative(product.stats.last_paid_at)}
          </span>
        </div>
      </div>
      <div className="cell c-actions actions">
        <button type="button" className="btn btn-small" onClick={onCreateLink} disabled={!product.active} aria-label={`Create link for ${product.name}`} title={product.active ? 'Create link' : 'Activate the product to create a link'}>
          <Icon name="link" />
          <span className="action-text">Create link</span>
        </button>
        <button type="button" className="btn btn-small" onClick={onEdit} aria-label={`Edit ${product.name}`} title="Edit">
          <Icon name="edit" />
          <span className="action-text">Edit</span>
        </button>
        {product.active ? (
          <button type="button" className="btn btn-small" onClick={onDeactivate} aria-label={`Deactivate ${product.name}`} title="Deactivate">
            <Icon name="power" />
            <span className="action-text">Deactivate</span>
          </button>
        ) : (
          <button
            type="button"
            className="btn btn-small"
            onClick={() => setActive.mutate({ product, active: true })}
            disabled={setActive.isPending}
            aria-label={`Activate ${product.name}`}
            title="Activate"
          >
            <Icon name="power" />
            <span className="action-text">Activate</span>
          </button>
        )}
      </div>
    </li>
  );
}

type Drawer = { kind: 'create' } | { kind: 'edit'; product: Product } | { kind: 'link'; product: Product } | null;

export function ProductsPage() {
  const { mode } = useMode();
  const [params, setParams] = useSearchParams();
  const navigate = useNavigate();
  const savedToast = useProductSavedToast();
  const linkToast = useLinkCreatedToast();
  const status = (['active', 'inactive', 'all'].includes(params.get('status') ?? '') ? params.get('status') : 'all') as ProductStatusFilter;
  const q = params.get('q') ?? '';
  const [search, setSearch] = useState(q);
  const [drawer, setDrawer] = useState<Drawer>(null);
  const [deactivate, setDeactivate] = useState<Product | null>(null);

  const update = (k: string, v: string) => {
    const next = new URLSearchParams(params);
    if (v && !(k === 'status' && v === 'all')) next.set(k, v);
    else next.delete(k);
    setParams(next, { replace: true });
  };

  useEffect(() => {
    const t = setTimeout(() => {
      if (search.trim() !== q) update('q', search.trim());
    }, 250);
    return () => clearTimeout(t);
  }, [search]);

  const filters = useMemo(() => ({ status, q }), [status, q]);
  const products = useProducts(filters);
  const rows = products.data?.pages.flatMap((p) => p.data) ?? [];
  const filtered = status !== 'all' || q !== '';

  return (
    <>
      <PageHead
        title="Products"
        lead="What you sell, how it is priced and where its money goes. Each product can have any number of payment links."
        actions={
          <button type="button" className="btn btn-primary" onClick={() => setDrawer({ kind: 'create' })}>
            <Icon name="plus" />
            Add product
          </button>
        }
      />

      <div className="toolbar" role="search">
        <Segmented<ProductStatusFilter>
          legend="Status"
          value={status}
          onChange={(v) => update('status', v)}
          options={[
            { value: 'active', label: 'Active' },
            { value: 'inactive', label: 'Inactive' },
            { value: 'all', label: 'All' },
          ]}
        />
        <div className="search">
          <Icon name="search" />
          <label htmlFor="product-search" className="sr-only">
            Search products
          </label>
          <input id="product-search" className="input" type="search" placeholder="Search by name" value={search} onChange={(e) => setSearch(e.target.value)} />
        </div>
      </div>

      {products.isPending ? (
        <Loading what="products" />
      ) : products.isError ? (
        <ErrorState error={products.error} onRetry={() => products.refetch()} />
      ) : rows.length === 0 && !filtered ? (
        <section className="card empty" aria-labelledby="empty-title">
          <h2 className="display" id="empty-title">
            Add your first product
          </h2>
          <p className="muted">A product is what you sell: a name, a price and the wallets its payments go to. Then share it with as many payment links as you like.</p>
          <p className="actions empty-actions">
            <button type="button" className="btn btn-primary" onClick={() => setDrawer({ kind: 'create' })}>
              <Icon name="plus" />
              Add product
            </button>
          </p>
        </section>
      ) : rows.length === 0 ? (
        <div className="card empty">
          <p className="subheading">No products match these filters.</p>
          <p className="muted">{mode === 'test' ? 'You are in Test mode: live products are not shown.' : 'Try another search or show all products.'}</p>
          <p className="actions empty-actions">
            <button
              type="button"
              className="btn"
              onClick={() => {
                setSearch('');
                setParams(new URLSearchParams(), { replace: true });
              }}
            >
              Clear filters
            </button>
          </p>
        </div>
      ) : (
        <>
          <ul className="rows" aria-label="Products">
            <li className="rows-head links products label" aria-hidden="true">
              <span>Product</span>
              <span>Payers can pay with</span>
              <span>Status</span>
              <span>Links</span>
              <span>Received</span>
              <span>Last payment</span>
              <span>Actions</span>
            </li>
            {rows.map((p) => (
              <ProductRow
                key={p.id}
                product={p}
                onCreateLink={() => setDrawer({ kind: 'link', product: p })}
                onEdit={() => setDrawer({ kind: 'edit', product: p })}
                onDeactivate={() => setDeactivate(p)}
              />
            ))}
          </ul>
          {products.hasNextPage ? (
            <div className="more">
              <button type="button" className="btn" onClick={() => products.fetchNextPage()} disabled={products.isFetchingNextPage}>
                {products.isFetchingNextPage ? 'Loading more products' : 'Show more products'}
              </button>
            </div>
          ) : null}
        </>
      )}

      {drawer?.kind === 'create' || drawer?.kind === 'edit' ? (
        <ProductDrawer
          product={drawer.kind === 'edit' ? drawer.product : undefined}
          onClose={() => setDrawer(null)}
          onSaved={(saved) => {
            setDrawer(null);
            savedToast(saved);
            if (saved.created) navigate(`/products/${saved.product.id}`);
          }}
        />
      ) : null}
      {drawer?.kind === 'link' ? (
        <LinkDrawer
          productId={drawer.product.id}
          lockProduct
          onClose={() => setDrawer(null)}
          onSaved={(saved) => {
            setDrawer(null);
            linkToast(saved);
          }}
        />
      ) : null}
      {deactivate ? <DeactivateProductDialog product={deactivate} onClose={() => setDeactivate(null)} /> : null}
    </>
  );
}
