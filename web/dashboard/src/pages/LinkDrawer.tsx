import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useEffect, useMemo, useRef, useState, type FormEvent } from 'react';
import { Dialog } from '../components/Dialog';
import { useDirtyGuard } from '../components/DirtyGuard';
import { Icon } from '../components/Icon';
import { useToast } from '../components/Toast';
import { Field, FieldError } from '../components/ui';
import { ApiError, newIdempotencyKey } from '../lib/api';
import { api } from '../lib/endpoints';
import { errorMessage } from '../lib/errors';
import { formatAmount } from '../lib/money';
import { productPrice, rangeText } from '../lib/format';
import { hasMoreOptions, isMoreOption } from '../lib/formFields';
import {
  createLinkBody,
  emptyLinkForm,
  formFromLink,
  LINK_FIELD_ORDER,
  linkPatch,
  linkPreview,
  validateLink,
  type AmountChoice,
  type LinkFormState,
} from '../lib/linkForm';
import { useMode } from '../lib/mode';
import { invalidateCatalog, keys, useProduct, useProducts, useSettings } from '../lib/queries';
import type { Link, Product } from '../lib/types';
import { CheckoutPreview } from './CheckoutPreview';
import { AmountInput, fieldOfParam, FormErrors, MetadataFields, MoreOptions } from './FormParts';
import { ProductDrawer } from './ProductDrawer';
import { PayoutReadOnly } from './ProductPayout';

interface Props {
  link?: Link;
  /** The product to start with. */
  productId?: string;
  /** Opened from a product: the link sells that one. */
  lockProduct?: boolean;
  onClose: () => void;
  onSaved: (link: Link, created: boolean) => void;
}

const PARAM_FIELDS = ['product_id', 'amount', 'name', 'expires_at', 'max_payments', 'success_url', 'metadata'];

export const fieldId = (key: string) => `link-${key.replace(/\./g, '-')}`;

/** "Between 5.00 and 100.00 USD, with 10.00, 25.00 and 50.00 offered as buttons." */
function payerChoiceHint(p: Pick<Product, 'min_amount' | 'max_amount' | 'suggested_amounts'>): string {
  const range = rangeText(p);
  const sug = (p.suggested_amounts ?? []).map((s) => formatAmount(s));
  if (sug.length === 0) return `${range}.`;
  const list = sug.length === 1 ? sug[0] : `${sug.slice(0, -1).join(', ')} and ${sug.at(-1)}`;
  return `${range}, with ${list} offered as buttons.`;
}

interface PickerProps {
  value: string;
  selected: Product | undefined;
  onChange: (p: Product) => void;
  onNew: () => void;
  error?: string;
}

/** Active products to choose from, searchable, with "New product" at the end. */
function ProductPicker({ value, selected, onChange, onNew, error }: PickerProps) {
  const [search, setSearch] = useState('');
  const [q, setQ] = useState('');
  useEffect(() => {
    const t = setTimeout(() => setQ(search.trim()), 200);
    return () => clearTimeout(t);
  }, [search]);
  const products = useProducts({ status: 'active', q });
  const found = products.data?.pages.flatMap((p) => p.data) ?? [];
  const missing = selected && !found.some((p) => p.id === selected.id) ? selected : undefined;
  const options = missing && !q ? [missing, ...found] : found;
  // Mixed into the results, the chosen product would read as a match for a search it fails.
  const kept = q ? missing : undefined;
  const errId = `${fieldId('product_id')}-error`;
  const option = (p: Product) => (
    <li key={p.id}>
      <label className="product-option">
        <input type="radio" name="link-product" value={p.id} checked={value === p.id} onChange={() => onChange(p)} />
        <span className="product-option-name">{p.name}</span>
        <span className="small muted amount">{productPrice(p)}</span>
      </label>
    </li>
  );
  return (
    <fieldset className="plain stack-sm" data-testid="product-picker">
      <legend>Product</legend>
      <p className="hint">The product this link sells: its name, price and where its money goes.</p>
      <div className="search">
        <Icon name="search" />
        <label htmlFor={fieldId('product_id')} className="sr-only">
          Search products
        </label>
        <input
          id={fieldId('product_id')}
          className="input"
          type="search"
          placeholder="Search products"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          aria-invalid={error ? true : undefined}
          aria-describedby={error ? errId : undefined}
          data-autofocus
        />
      </div>
      {products.isPending ? (
        <p className="small muted">Loading products.</p>
      ) : products.isError ? (
        <ErrorLine error={products.error} />
      ) : options.length === 0 ? (
        <p className="small muted">{q ? 'No active product matches this search.' : 'No active products yet. Add one to sell it with this link.'}</p>
      ) : (
        <ul className="product-options" aria-label="Products">
          {options.map(option)}
        </ul>
      )}
      {kept && !products.isPending && (
        <div className="stack-sm" data-testid="product-picker-selected">
          <p className="small muted">Selected</p>
          <ul className="product-options" aria-label="Selected product">
            {option(kept)}
          </ul>
        </div>
      )}
      <div>
        <button type="button" className="btn btn-small" onClick={onNew}>
          <Icon name="plus" />
          New product
        </button>
      </div>
      <FieldError id={errId} message={error} />
    </fieldset>
  );
}

function ErrorLine({ error }: { error: unknown }) {
  return <p className="small muted">{errorMessage(error)}</p>;
}

export function LinkDrawer({ link, productId, lockProduct = false, onClose, onSaved }: Props) {
  const { mode } = useMode();
  const qc = useQueryClient();
  const toast = useToast();
  const settings = useSettings();
  const fee = settings.data?.fee ?? null;
  const editing = Boolean(link);
  const locked = editing || lockProduct;
  const [form, setForm] = useState<LinkFormState>(() => (link ? formFromLink(link) : emptyLinkForm(productId)));
  const [initial] = useState(form);
  const guard = useDirtyGuard({ initial, value: form, onClose, title: editing ? 'Discard your changes?' : 'Discard this link?' });
  const product = useProduct(form.product_id || null);
  const p = product.data && product.data.id === form.product_id ? product.data : undefined;
  const [moreOpen, setMoreOpen] = useState(() => hasMoreOptions(form));
  const [serverFields, setServerFields] = useState<Record<string, string>>({});
  const [checked, setChecked] = useState(false);
  const [touched, setTouched] = useState<Set<string>>(() => new Set());
  const [serverError, setServerError] = useState<string | null>(null);
  const [productDrawer, setProductDrawer] = useState<'new' | 'edit' | null>(null);
  const [focusTarget, setFocusTarget] = useState<{ id: string; n: number } | null>(null);
  useEffect(() => {
    if (focusTarget) document.getElementById(focusTarget.id)?.focus();
  }, [focusTarget]);
  const focusLater = (id: string) => setFocusTarget((t) => ({ id, n: (t?.n ?? 0) + 1 }));
  const keysByBody = useRef(new Map<string, string>());
  const testmode = link ? link.livemode === false : mode === 'test';

  const check = useMemo(() => validateLink(form, { product: p, fee, originalExpiresAt: link?.expires_at ?? null }), [form, p, fee, link]);
  const errors = check.errors;
  // Without the product's price and metadata the link cannot be checked yet.
  const input = p ? check.input : null;
  const model = useMemo(() => linkPreview(p ?? null, form), [p, form]);
  const show = (k: string) => serverFields[k] ?? (checked || touched.has(k) ? errors[k] : undefined);
  const touch = (k: string) => () => setTouched((t) => (t.has(k) ? t : new Set(t).add(k)));
  const set = <K extends keyof LinkFormState>(k: K, v: LinkFormState[K]) => {
    setForm((f) => ({ ...f, [k]: v }));
    setServerError(null);
    // A server refusal clears once the field it is about changes (the amount also with its choice).
    const gone = k === 'amount_choice' ? ['amount_choice', 'amount'] : [k as string];
    setServerFields((s) => (gone.some((g) => g in s) ? Object.fromEntries(Object.entries(s).filter(([x]) => !gone.includes(x))) : s));
  };
  const pick = (next: Product) => {
    setForm((f) => (f.product_id === next.id ? f : { ...f, product_id: next.id, amount_choice: 'payer', amount: '' }));
    setServerFields({});
    setServerError(null);
  };

  const reveal = (key: string) => {
    if (isMoreOption(key)) setMoreOpen(true);
    focusLater(fieldId(key));
  };

  const save = useMutation({
    mutationFn: async () => {
      if (!input) throw new Error('invalid');
      if (link) return { saved: await api.updateLink(mode, link.id, linkPatch(link, input)), created: false };
      const body = createLinkBody(input);
      const sig = JSON.stringify(body);
      // Same body, same key: a retry after a dropped connection returns the link already made.
      let key = keysByBody.current.get(sig);
      if (!key) {
        key = newIdempotencyKey();
        keysByBody.current.set(sig, key);
      }
      return { saved: await api.createLink(mode, body, key), created: true };
    },
    onSuccess: ({ saved, created }) => {
      qc.setQueryData(keys.link(mode, saved.id), saved);
      invalidateCatalog(qc);
      guard.release();
      onSaved(saved, created);
    },
    onError: (e) => {
      if (e instanceof ApiError && e.status >= 400 && e.status < 500) {
        const key =
          e.code === 'amount_below_minimum' || e.code === 'amount_above_maximum' ? 'amount' : e.code === 'product_not_found' ? 'product_id' : fieldOfParam(e.param, PARAM_FIELDS);
        if (key) {
          setServerFields({ [key]: e.message });
          reveal(key);
          return;
        }
      }
      setServerError(errorMessage(e));
    },
  });

  const submit = (e?: FormEvent) => {
    e?.preventDefault();
    setChecked(true);
    const first = LINK_FIELD_ORDER.find((k) => errors[k]) ?? Object.keys(errors)[0];
    if (first) return reveal(first);
    if (!input) return;
    save.mutate();
  };

  const shownCount = Object.keys({ ...(checked ? errors : {}), ...serverFields }).length;
  const tz = Intl.DateTimeFormat().resolvedOptions().timeZone;
  const variable = p?.price_type === 'open';
  const chains = p?.payout ? Object.keys(p.payout).filter((c) => (p.payout?.[c]?.length ?? 0) > 0) : undefined;

  return (
    <>
      <Dialog
        variant="drawer"
        title={editing ? 'Edit payment link' : 'Create payment link'}
        onClose={guard.requestClose}
        busy={save.isPending}
        footer={
          <>
            <button type="button" className="btn" onClick={guard.requestClose} disabled={save.isPending}>
              Cancel
            </button>
            <button type="submit" form="link-form" className="btn btn-primary" disabled={save.isPending}>
              {save.isPending ? <span className="spinner" aria-hidden="true" /> : null}
              {editing ? 'Save changes' : 'Create link'}
            </button>
          </>
        }
      >
        <form id="link-form" className="stack" onSubmit={submit} noValidate>
          <FormErrors count={shownCount} serverError={serverError} />

          {locked ? (
            <div className="recap" data-testid="picked-product">
              <div className="recap-text">
                <p className="label">Product</p>
                <p className="recap-line">{p ? `${p.name} · ${productPrice(p)}` : product.isError ? 'This product could not be loaded.' : 'Loading the product.'}</p>
                {editing ? <p className="small muted">A link always sells the same product. To sell another one, create a new link.</p> : null}
              </div>
            </div>
          ) : (
            <ProductPicker value={form.product_id} selected={p} onChange={pick} onNew={() => setProductDrawer('new')} error={show('product_id')} />
          )}

          {variable && p ? (
            <div className="stack-md">
              <fieldset className="plain">
                <legend>Amount</legend>
                <div className="kind-switch">
                  {(['payer', 'set'] as AmountChoice[]).map((c) => (
                    <label key={c} className="kind-option">
                      <input type="radio" name="amount_choice" value={c} checked={form.amount_choice === c} onChange={() => set('amount_choice', c)} />
                      <span className="kind-title">{c === 'payer' ? 'Let the payer choose' : 'Set the amount'}</span>
                      <span className="small muted">{c === 'payer' ? payerChoiceHint(p) : 'One amount for every payer of this link, such as an invoice or a quote.'}</span>
                    </label>
                  ))}
                </div>
              </fieldset>
              {form.amount_choice === 'set' ? (
                <Field id={fieldId('amount')} label="Amount for this link" hint={`Payers send exactly this amount. It must be within the product's range: ${rangeText(p).replace('From', 'from')}.`} error={show('amount')}>
                  {(fp) => <AmountInput p={fp} placeholder={p.min_amount ? formatAmount(p.min_amount) : '25.00'} value={form.amount} onChange={(v) => set('amount', v)} onBlur={touch('amount')} />}
                </Field>
              ) : null}
            </div>
          ) : null}

          <Field id={fieldId('name')} label="Label" optional hint="Only you see this, such as Instagram bio or Invoice ACME #12. Payers see the product name." error={show('name')}>
            {(fp) => <input {...fp} className="input" value={form.name} maxLength={240} onChange={(e) => set('name', e.target.value)} onBlur={touch('name')} data-autofocus={locked || undefined} />}
          </Field>

          <div className="stack-md">
            <h3 className="section-title">Availability</h3>
            <div className="form-grid-2">
              <Field id={fieldId('expires_at')} label="Expires at" optional hint={`After this time the link opens no new payments. Your time zone: ${tz}.`} error={show('expires_at')}>
                {(fp) => <input {...fp} className="input" type="datetime-local" value={form.expires_at} onChange={(e) => set('expires_at', e.target.value)} onBlur={touch('expires_at')} />}
              </Field>
              <Field id={fieldId('max_payments')} label="Payment limit" optional hint="After this many paid payments the link stops. Leave empty for no limit." error={show('max_payments')}>
                {(fp) => <input {...fp} className="input num" inputMode="numeric" autoComplete="off" placeholder="No limit" value={form.max_payments} onChange={(e) => set('max_payments', e.target.value)} onBlur={touch('max_payments')} />}
              </Field>
            </div>
          </div>

          <MoreOptions open={moreOpen} onToggle={() => setMoreOpen((o) => !o)} id="link-more" summary="Return address and extra metadata">
            <Field
              id={fieldId('success_url')}
              label="Return address after paying"
              optional
              hint={p?.success_url ? `Replaces the product's return address, ${p.success_url}, for this link.` : 'Once paid, the payer can go back to this page, such as https://acme.com/thanks.'}
              error={show('success_url')}
            >
              {(fp) => <input {...fp} className="input" type="url" inputMode="url" autoComplete="off" placeholder="https://" value={form.success_url} onChange={(e) => set('success_url', e.target.value)} onBlur={touch('success_url')} />}
            </Field>
            <MetadataFields
              rows={form.metadata}
              onChange={(rows) => set('metadata', rows)}
              show={show}
              touch={touch}
              fieldId={fieldId}
              legend="Extra metadata"
              hint="Added to the product's metadata on every payment from this link and sent in webhooks. A key here wins over the product's. Payers never see it."
            />
          </MoreOptions>

          {p ? <PayoutReadOnly payout={p.payout} onEdit={() => setProductDrawer('edit')} /> : null}

          {editing ? (
            <p className="note" data-emphasis>
              <Icon name="info" />
              <span>Changes apply to new payments only. Payments already opened keep their amount.</span>
            </p>
          ) : null}
        </form>
        <div className="preview-col">
          <CheckoutPreview model={model} testmode={testmode} accepted={settings.data?.accepted_assets} chains={chains} />
        </div>
        {guard.confirm}
      </Dialog>
      {productDrawer === 'new' ? (
        <ProductDrawer
          offerLink={false}
          onClose={() => setProductDrawer(null)}
          onSaved={({ product: made }) => {
            setProductDrawer(null);
            qc.setQueryData(keys.product(mode, made.id), made);
            pick(made);
            toast.show(`${made.name} created`);
          }}
        />
      ) : null}
      {productDrawer === 'edit' && p ? (
        <ProductDrawer
          product={p}
          initialStep="payout"
          onClose={() => setProductDrawer(null)}
          onSaved={() => {
            setProductDrawer(null);
            toast.show('Changes saved');
          }}
        />
      ) : null}
    </>
  );
}
