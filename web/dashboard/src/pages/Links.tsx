import { useEffect, useMemo, useState } from 'react';
import { Link as RouterLink, useNavigate, useSearchParams } from 'react-router-dom';
import { Icon } from '../components/Icon';
import { useToast } from '../components/Toast';
import { Amounts, CopyButton, ErrorState, Loading, PageHead, Pill, Segmented } from '../components/ui';
import type { LinkStatusFilter } from '../lib/endpoints';
import { formatConversion, formatDateTime, formatRelative, linkPrice, linkTitle } from '../lib/format';
import { useMode } from '../lib/mode';
import { useLinks, useProducts } from '../lib/queries';
import { linkPill } from '../lib/status';
import type { Link } from '../lib/types';
import { DeactivateDialog, QrDialog, useSetActive } from './LinkActions';
import { LinkDrawer } from './LinkDrawer';

function LinkRow({ link, showProduct = true, onQr, onEdit, onDeactivate }: { link: Link; showProduct?: boolean; onQr: () => void; onEdit: () => void; onDeactivate: () => void }) {
  const setActive = useSetActive();
  const pill = linkPill(link);
  const title = linkTitle(link);
  return (
    <li className="data-row link-row" data-testid="link-row" data-link-id={link.id}>
      <div className="cell c-name">
        <RouterLink to={`/links/${link.id}`} className="row-link">
          {title}
        </RouterLink>
        <div className="meta">
          {showProduct && link.name ? (
            <RouterLink to={`/products/${link.product_id}`} className="small link-product" data-testid="link-product">
              {link.product.name}
            </RouterLink>
          ) : null}
          {!showProduct && !link.name ? <span className="small muted">No label</span> : null}
          <span className="amount small" data-testid="link-price">
            {linkPrice(link)}
          </span>
          {link.amount ? <span className="tag">Set amount</span> : null}
        </div>
      </div>
      <div className="cell c-status">
        <span className="cell-label">Status</span>
        <Pill pill={pill} />
        {link.unavailable_reason === 'reserved' ? <span className="small muted block">Payments in progress</span> : null}
      </div>
      <div className="c-stats">
        <div className="cell">
          <span className="cell-label">Opened → paid</span>
          <span className="flow mono num" aria-label={`${link.stats.opened} opened, ${link.stats.paid} paid`}>
            <span>{link.stats.opened.toLocaleString('en-US')}</span>
            <span className="arrow" aria-hidden="true">
              →
            </span>
            <span>{link.stats.paid.toLocaleString('en-US')}</span>
          </span>
          <span className="small muted"> {formatConversion(link.stats.conversion, link.stats.opened, link.stats.paid)}</span>
        </div>
        <div className="cell c-received">
          <span className="cell-label">Received</span>
          <Amounts items={link.stats.received} />
        </div>
        <div className="cell">
          <span className="cell-label">Last payment</span>
          <span className="small" title={formatDateTime(link.stats.last_paid_at)}>
            {formatRelative(link.stats.last_paid_at)}
          </span>
        </div>
      </div>
      <div className="cell c-actions actions">
        <CopyButton value={link.url} what="link" label="Copy link" ariaLabel={`Copy link for ${title}`} />
        <button type="button" className="btn btn-small" onClick={onQr} aria-label={`Show QR code for ${title}`} title="QR code">
          <Icon name="qr" />
          <span className="action-text">QR</span>
        </button>
        <button type="button" className="btn btn-small" onClick={onEdit} aria-label={`Edit ${title}`} title="Edit">
          <Icon name="edit" />
          <span className="action-text">Edit</span>
        </button>
        {link.active ? (
          <button type="button" className="btn btn-small" onClick={onDeactivate} aria-label={`Deactivate ${title}`} title="Deactivate">
            <Icon name="power" />
            <span className="action-text">Deactivate</span>
          </button>
        ) : (
          <button
            type="button"
            className="btn btn-small"
            onClick={() => setActive.mutate({ link, active: true })}
            disabled={setActive.isPending}
            aria-label={`Activate ${title}`}
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

/** The links table, shared by the links page and a product's page. */
export function LinkRows({ links, label, showProduct = true, onQr, onEdit, onDeactivate }: { links: Link[]; label: string; showProduct?: boolean; onQr: (l: Link) => void; onEdit: (l: Link) => void; onDeactivate: (l: Link) => void }) {
  return (
    <ul className="rows" aria-label={label}>
      <li className="rows-head links label" aria-hidden="true">
        <span>Link</span>
        <span>Status</span>
        <span>Opened → paid</span>
        <span>Received</span>
        <span>Last payment</span>
        <span>Actions</span>
      </li>
      {links.map((l) => (
        <LinkRow key={l.id} link={l} showProduct={showProduct} onQr={() => onQr(l)} onEdit={() => onEdit(l)} onDeactivate={() => onDeactivate(l)} />
      ))}
    </ul>
  );
}

/** Every product of the mode, for a filter. */
export function ProductFilter({ value, onChange, id }: { value: string; onChange: (v: string) => void; id: string }) {
  const products = useProducts({ status: 'all' });
  const list = products.data?.pages.flatMap((p) => p.data) ?? [];
  return (
    <div className="field">
      <label htmlFor={id} className="sr-only">
        Product
      </label>
      <select id={id} className="select" value={value} onChange={(e) => onChange(e.target.value)}>
        <option value="">Every product</option>
        {list.map((p) => (
          <option key={p.id} value={p.id}>
            {p.name}
          </option>
        ))}
        {value && !list.some((p) => p.id === value) ? <option value={value}>{value}</option> : null}
      </select>
    </div>
  );
}

export function LinksPage() {
  const { mode } = useMode();
  const [params, setParams] = useSearchParams();
  const navigate = useNavigate();
  const toast = useToast();
  const status = (['live', 'inactive', 'all'].includes(params.get('status') ?? '') ? params.get('status') : 'all') as LinkStatusFilter;
  const productId = params.get('product_id') ?? '';
  const q = params.get('q') ?? '';
  const [search, setSearch] = useState(q);
  const [drawer, setDrawer] = useState<{ kind: 'create' } | { kind: 'edit'; link: Link } | null>(null);
  const [qr, setQr] = useState<Link | null>(null);
  const [deactivate, setDeactivate] = useState<Link | null>(null);

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

  const filters = useMemo(() => ({ status, product_id: productId, q }), [status, productId, q]);
  const links = useLinks(filters);
  const rows = links.data?.pages.flatMap((p) => p.data) ?? [];
  const filtered = status !== 'all' || productId !== '' || q !== '';

  return (
    <>
      <PageHead
        title="Payment links"
        lead="Each link sells one of your products at its own address. Live links come first."
        actions={
          <button type="button" className="btn btn-primary" onClick={() => setDrawer({ kind: 'create' })}>
            <Icon name="plus" />
            New link
          </button>
        }
      />

      <div className="toolbar" role="search">
        <Segmented<LinkStatusFilter>
          legend="Status"
          value={status}
          onChange={(v) => update('status', v)}
          options={[
            { value: 'live', label: 'Live' },
            { value: 'inactive', label: 'Inactive' },
            { value: 'all', label: 'All' },
          ]}
        />
        <ProductFilter id="link-product-filter" value={productId} onChange={(v) => update('product_id', v)} />
        <div className="search">
          <Icon name="search" />
          <label htmlFor="link-search" className="sr-only">
            Search links
          </label>
          <input id="link-search" className="input" type="search" placeholder="Search by label or product" value={search} onChange={(e) => setSearch(e.target.value)} />
        </div>
      </div>

      {links.isPending ? (
        <Loading what="payment links" />
      ) : links.isError ? (
        <ErrorState error={links.error} onRetry={() => links.refetch()} />
      ) : rows.length === 0 && !filtered ? (
        <section className="card empty" aria-labelledby="empty-title">
          <h2 className="display" id="empty-title">
            Create your first payment link
          </h2>
          <p className="muted">A link sells one of your products. Share it anywhere: every payment goes straight to your wallets.</p>
          <p className="actions empty-actions">
            <button type="button" className="btn btn-primary" onClick={() => setDrawer({ kind: 'create' })}>
              <Icon name="plus" />
              New link
            </button>
            <RouterLink to="/products" className="btn">
              See products
            </RouterLink>
          </p>
        </section>
      ) : rows.length === 0 ? (
        <div className="card empty">
          <p className="subheading">No links match these filters.</p>
          <p className="muted">{mode === 'test' ? 'You are in Test mode: live links are not shown.' : 'Try another search or show all links.'}</p>
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
          <LinkRows links={rows} label="Payment links" onQr={setQr} onEdit={(l) => setDrawer({ kind: 'edit', link: l })} onDeactivate={setDeactivate} />
          {links.hasNextPage ? (
            <div className="more">
              <button type="button" className="btn" onClick={() => links.fetchNextPage()} disabled={links.isFetchingNextPage}>
                {links.isFetchingNextPage ? 'Loading more links' : 'Show more links'}
              </button>
            </div>
          ) : null}
        </>
      )}

      {drawer ? (
        <LinkDrawer
          link={drawer.kind === 'edit' ? drawer.link : undefined}
          productId={productId || undefined}
          onClose={() => setDrawer(null)}
          onSaved={(saved, created) => {
            setDrawer(null);
            toast.show(created ? `${linkTitle(saved)} link created` : 'Changes saved');
            if (created) navigate(`/links/${saved.id}`);
          }}
        />
      ) : null}
      {qr ? <QrDialog link={qr} onClose={() => setQr(null)} /> : null}
      {deactivate ? <DeactivateDialog link={deactivate} onClose={() => setDeactivate(null)} /> : null}
    </>
  );
}
