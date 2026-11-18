import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useEffect, useMemo, useRef, useState, type FormEvent } from 'react';
import { Dialog } from '../components/Dialog';
import { useDirtyGuard } from '../components/DirtyGuard';
import { Icon } from '../components/Icon';
import { ApiError, newIdempotencyKey } from '../lib/api';
import { api } from '../lib/endpoints';
import { errorMessage } from '../lib/errors';
import { hasMoreOptions, isMoreOption } from '../lib/formFields';
import { useMode } from '../lib/mode';
import { chooseNetwork, choiceOf, effectiveMethods, networksBody, networksErrors, prefillNetworks, toggleAsset, type NetworksForm } from '../lib/networkAssets';
import { NONE_KEY, payoutChanges, payoutErrorKey, type PayoutChange, prefillPayout, rowKey, validatePayout } from '../lib/payout';
import { activeLinksIn, asksAboutLinks, isLinksDecisionRequired } from '../lib/productLinks';
import {
  createProductBody,
  emptyProductForm,
  formFromProduct,
  productPatch,
  productPreview,
  recapLine,
  stepOfField,
  takenChains,
  validateProduct,
  type ProductFormState,
  type ProductStep,
} from '../lib/productForm';
import { invalidateCatalog, keys, useSettings } from '../lib/queries';
import type { Link, LinksDecision, PriceType, Product } from '../lib/types';
import { CheckoutPreview } from './CheckoutPreview';
import { fieldOfParam, FormErrors } from './FormParts';
import { LinksDecisionDialog } from './LinksDecisionDialog';
import { productFieldId, ProductDetailsFields } from './ProductFields';
import { NO_NETWORK_LEFT, ProductNetworks } from './ProductNetworks';
import { PayoutConfirmDialog, PayoutEditor, PayoutRecap, PayoutSummary, usePayoutData } from './ProductPayout';

export interface ProductSaved {
  product: Product;
  created: boolean;
  /** The link made with "Also create a payment link", or null. */
  link: Link | null;
  /** Why that link could not be made, when the product was. */
  linkError: unknown;
}

interface Props {
  product?: Product;
  initialPriceType?: PriceType;
  /** Editing opens on this step. */
  initialStep?: ProductStep;
  /** Offers "Also create a payment link" when creating; off when the caller makes the link itself. */
  offerLink?: boolean;
  onClose: () => void;
  onSaved: (saved: ProductSaved) => void;
}

const FIELD_ORDER = ['name', 'description', 'amount', 'min_amount', 'max_amount', 'suggested.0', 'suggested.1', 'suggested.2', 'success_url'];
const PARAM_FIELDS = ['name', 'description', 'amount', 'min_amount', 'max_amount', 'success_url', 'metadata'];

const STEPS: { id: ProductStep; label: string }[] = [
  { id: 'details', label: 'Product details' },
  { id: 'payout', label: 'How you get paid' },
];


/** Nothing left for payers to pay with, after Settings and the choices. */
const NO_NETWORK_KEY = 'networks.payers';

/** Where to put the cursor for an error: a network's sum or count goes to its first row. */
function focusId(key: string): string {
  if (key === NO_NETWORK_KEY) return 'networks-title';
  const m = /^payout\.([a-z0-9-]+)\.(sum|rows)$/.exec(key);
  if (m) return productFieldId(rowKey(m[1]!, 0, m[2] === 'sum' ? 'share' : 'wallet'));
  return productFieldId(key);
}

export function ProductDrawer({ product, initialPriceType = 'fixed', initialStep = 'details', offerLink = true, onClose, onSaved }: Props) {
  const { mode } = useMode();
  const qc = useQueryClient();
  const settings = useSettings();
  const payoutData = usePayoutData();
  const fee = settings.data?.fee ?? null;
  const editing = Boolean(product);
  const [form, setForm] = useState<ProductFormState>(() => {
    if (product) return formFromProduct(product);
    const f = emptyProductForm(initialPriceType);
    if (!payoutData.ready) return f;
    const payout = prefillPayout(payoutData.wallets, payoutData.chains);
    return { ...f, payout, networks: prefillNetworks(payout, payoutData.chains) };
  });
  // What the drawer opened with, the prefilled payout included: closing asks only after a change.
  const [initial, setInitial] = useState(form);
  // A new product starts with each network's oldest wallet at 100%, once the wallets are known.
  const prefilled = useRef(editing || payoutData.ready);
  useEffect(() => {
    if (prefilled.current || !payoutData.ready) return;
    prefilled.current = true;
    const payout = prefillPayout(payoutData.wallets, payoutData.chains);
    const networks = prefillNetworks(payout, payoutData.chains);
    setForm((f) => ({ ...f, payout, networks }));
    setInitial((f) => ({ ...f, payout, networks }));
  }, [payoutData.ready, payoutData.wallets, payoutData.chains]);
  const guard = useDirtyGuard({ initial, value: form, onClose, title: editing ? 'Discard your changes?' : 'Discard this product?' });
  const [alsoLink, setAlsoLink] = useState(true);
  // Only a saved product has the two steps; a new one is a single form.
  const [step, setStep] = useState<ProductStep>(editing ? initialStep : 'details');
  // A new product's payout shows as a recap; the split editor opens in place from "Change".
  const [changing, setChanging] = useState(false);
  const [moreOpen, setMoreOpen] = useState(() => hasMoreOptions(form));
  const [serverFields, setServerFields] = useState<Record<string, string>>({});
  // Steps the merchant tried to leave or submit: each of their errors shows, touched or not.
  const [checked, setChecked] = useState<Set<ProductStep>>(() => new Set());
  const [touched, setTouched] = useState<Set<string>>(() => new Set());
  const [serverError, setServerError] = useState<string | null>(null);
  // A price or payout change of a product with active links asks first what happens to them.
  const [decision, setDecision] = useState<{ count: number | null } | null>(null);
  // Changing where a saved product's money goes asks first, with the old and the new destination.
  const [payoutConfirm, setPayoutConfirm] = useState<PayoutChange[] | null>(null);
  // Focus moves once the step, or the options it points into, has rendered.
  const [focusTarget, setFocusTarget] = useState<{ id: string; n: number } | null>(null);
  useEffect(() => {
    if (focusTarget) document.getElementById(focusTarget.id)?.focus();
  }, [focusTarget]);
  const focusLater = (id: string) => setFocusTarget((t) => ({ id, n: (t?.n ?? 0) + 1 }));
  const keysByBody = useRef(new Map<string, string>());
  const testmode = product ? product.livemode === false : mode === 'test';

  const details = useMemo(() => validateProduct(form, { fee }), [form, fee]);
  const payoutCheck = useMemo(() => validatePayout(form.payout, payoutData.ready ? payoutData.wallets : undefined), [form.payout, payoutData.ready, payoutData.wallets]);
  const accepted = useMemo(() => settings.data?.accepted_assets ?? [], [settings.data]);
  // A network with no wallet rows is Off, whatever else the form says about it.
  const networks = useMemo<NetworksForm>(() => Object.fromEntries(Object.keys(form.payout).map((c) => [c, (form.payout[c]?.length ?? 0) > 0 ? choiceOf(form.networks, c) : choiceOf({}, c)])), [form.payout, form.networks]);
  const hidden = useMemo(() => Object.keys(networks).filter((c) => networks[c]!.accept === 'off'), [networks]);
  const networkErrors = useMemo(() => {
    const out = networksErrors(networks);
    const taking = Object.keys(networks).some((c) => networks[c]!.accept !== 'off');
    if (settings.isSuccess && taking && Object.keys(out).length === 0 && effectiveMethods(networks, accepted, Object.keys(networks)).length === 0) out[NO_NETWORK_KEY] = NO_NETWORK_LEFT;
    return out;
  }, [networks, accepted, settings.isSuccess]);
  const errors = useMemo(() => ({ ...details.errors, ...payoutCheck.errors, ...networkErrors }), [details.errors, payoutCheck.errors, networkErrors]);
  const input = details.input && payoutCheck.input && payoutData.ready && Object.keys(networkErrors).length === 0 ? { ...details.input, payout: payoutCheck.input, networks: networksBody(networks) } : null;
  const model = useMemo(() => productPreview(form), [form]);
  const show = (k: string) => serverFields[k] ?? (checked.has(stepOfField(k)) || touched.has(k) ? errors[k] : undefined);
  const touch = (k: string) => () => setTouched((t) => (t.has(k) ? t : new Set(t).add(k)));
  const set = <K extends keyof ProductFormState>(k: K, v: ProductFormState[K]) => {
    setForm((f) => ({ ...f, [k]: v }));
    setServerError(null);
    // A server refusal clears once what it is about changes.
    const about = (x: string) => (k === 'payout' || k === 'networks' ? stepOfField(x) === 'payout' : x === k || x.startsWith(`${k}.`));
    setServerFields((s) => (Object.keys(s).some(about) ? Object.fromEntries(Object.entries(s).filter(([x]) => !about(x))) : s));
  };

  const choose = (chain: string, accept: 'all' | 'some' | 'off') => {
    const next = chooseNetwork(form, chain, accept, payoutData.wallets, accepted);
    set('networks', next.networks);
    set('payout', next.payout);
  };
  const toggle = (chain: string, asset: string) => set('networks', toggleAsset(form.networks, chain, asset));

  const firstError = (st: ProductStep): string | undefined => {
    const ks = Object.keys(errors).filter((k) => stepOfField(k) === st);
    if (st === 'details') return FIELD_ORDER.find((k) => errors[k]) ?? ks[0];
    return ks.find((k) => k !== NONE_KEY) ?? ks[0];
  };
  const reveal = (key: string) => {
    if (isMoreOption(key)) setMoreOpen(true);
    if (!editing && (key === 'payout' || key.startsWith('payout.'))) setChanging(true);
    setStep(editing ? stepOfField(key) : 'details');
    focusLater(focusId(key));
  };
  const toDetails = () => {
    setStep('details');
    focusLater(productFieldId('name'));
  };
  const toPayout = () => {
    setStep('payout');
    focusLater('payout-title');
  };

  const keyFor = (sig: string) => {
    // Same body, same key: a retry after a dropped connection returns what was already made.
    let key = keysByBody.current.get(sig);
    if (!key) {
      key = newIdempotencyKey();
      keysByBody.current.set(sig, key);
    }
    return key;
  };

  const save = useMutation({
    mutationFn: async (links?: LinksDecision): Promise<ProductSaved> => {
      if (!input) throw new Error('invalid');
      if (product) {
        const patch = productPatch(product, input);
        return { product: await api.updateProduct(mode, product.id, links ? { ...patch, links } : patch), created: false, link: null, linkError: null };
      }
      const body = createProductBody(input);
      const saved = await api.createProduct(mode, body, keyFor(JSON.stringify(body)));
      if (!(offerLink && alsoLink)) return { product: saved, created: true, link: null, linkError: null };
      try {
        const link = await api.createLink(mode, { product_id: saved.id }, keyFor(`link:${saved.id}`));
        return { product: saved, created: true, link, linkError: null };
      } catch (e) {
        return { product: saved, created: true, link: null, linkError: e };
      }
    },
    onSuccess: (result) => {
      setDecision(null);
      qc.setQueryData(keys.product(mode, result.product.id), result.product);
      invalidateCatalog(qc);
      guard.release();
      onSaved(result);
    },
    onError: (e) => {
      // A link turned on since the drawer opened: the server asks the same question.
      if (isLinksDecisionRequired(e)) {
        setDecision({ count: activeLinksIn(e) });
        return;
      }
      setDecision(null);
      if (e instanceof ApiError && e.status === 400) {
        // A refused payout is shown on the row, total or network it is about; a field the server names, on that field.
        const net = e.param ? /^networks\.([a-z0-9-]+)/.exec(e.param) : null;
        const key = net && form.payout[net[1]!] ? `networks.${net[1]}` : payoutErrorKey(e, form.payout, payoutData.wallets) ?? fieldOfParam(e.param, PARAM_FIELDS);
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
    if (!payoutData.ready) return;
    setChecked(new Set<ProductStep>(['details', 'payout']));
    if (!input) {
      const other: ProductStep = step === 'details' ? 'payout' : 'details';
      const first = firstError(step) ?? firstError(other);
      if (first) reveal(first);
      return;
    }
    if (product) {
      const changes = payoutChanges(product.payout, input.payout, payoutData.wallets);
      if (changes.length > 0) {
        setPayoutConfirm(changes);
        return;
      }
    }
    proceed();
  };
  const proceed = () => {
    if (product && input && asksAboutLinks(product, productPatch(product, input))) {
      setDecision({ count: null });
      return;
    }
    save.mutate(undefined);
  };

  // Saving needs the wallets: until they load the payout cannot be checked, so the save waits.
  const waitingForWallets = !payoutData.ready;
  const shown = { ...(checked.has(step) ? errors : {}), ...serverFields };
  const stepErrors = Object.keys(shown).filter((k) => !editing || stepOfField(k) === step).length;
  const onPayout = step === 'payout';
  const networksSection = <ProductNetworks networks={networks} payout={form.payout} accepted={accepted} wallets={payoutData.wallets} chains={payoutData.chains} ready={payoutData.ready && settings.isSuccess} onChoose={choose} onToggle={toggle} error={show} />;

  return (
    <Dialog
      variant="drawer"
      title={editing ? 'Edit product' : 'Add product'}
      onClose={guard.requestClose}
      busy={save.isPending}
      footer={
        <>
          {onPayout || (!editing && changing) ? <PayoutSummary value={form.payout} data={payoutData} hidden={hidden} /> : null}
          {waitingForWallets ? (
            <p className={payoutData.failed ? 'payout-summary small error' : 'payout-summary small'} id="product-save-wait" role={payoutData.failed ? 'alert' : 'status'}>
              {payoutData.failed ? <Icon name="alert" /> : null}
              <span>{payoutData.failed ? 'Your wallets and payout settings could not be loaded. Close this and try again.' : 'Loading your wallets…'}</span>
            </p>
          ) : null}
          <button type="button" className="btn" onClick={guard.requestClose} disabled={save.isPending}>
            Cancel
          </button>
          <button type="submit" form="product-form" className="btn btn-primary" disabled={save.isPending || waitingForWallets} aria-describedby={waitingForWallets ? 'product-save-wait' : undefined}>
            {save.isPending || (waitingForWallets && !payoutData.failed) ? <span className="spinner" aria-hidden="true" /> : null}
            {editing ? 'Save changes' : 'Create product'}
          </button>
        </>
      }
    >
      <form id="product-form" className="stack" onSubmit={submit} noValidate>
        {editing ? (
          <ol className="steps" aria-label="Steps">
            {STEPS.map((s, i) => (
              <li key={s.id}>
                <button type="button" className="step" aria-current={s.id === step ? 'step' : undefined} onClick={s.id === 'details' ? toDetails : toPayout} disabled={save.isPending}>
                  <span className="step-num" aria-hidden="true">
                    {i + 1}
                  </span>
                  <span>{s.label}</span>
                </button>
              </li>
            ))}
          </ol>
        ) : null}

        <FormErrors count={stepErrors} serverError={serverError} />

        {onPayout ? (
          <>
            <div className="recap" data-testid="product-recap">
              <div className="recap-text">
                <p className="label">Product</p>
                <p className="recap-line">{recapLine(model)}</p>
              </div>
              <button type="button" className="btn btn-small btn-ghost" onClick={toDetails}>
                <Icon name="edit" />
                Edit details
              </button>
            </div>

            {networksSection}

            <PayoutEditor value={form.payout} onChange={(next) => set('payout', next)} data={payoutData} error={show} onTouch={(k) => touch(k)()} hidden={hidden} />

            <p className="note" data-emphasis>
              <Icon name="info" />
              <span>Payout changes apply to new payments only, from every link of this product. Payments already opened keep their wallets.</span>
            </p>
          </>
        ) : (
          <>
            <ProductDetailsFields form={form} set={set} show={show} touch={touch} editing={editing} fee={fee} moreOpen={moreOpen} onToggleMore={() => setMoreOpen((o) => !o)} />

            {editing ? (
              <p className="note" data-emphasis>
                <Icon name="info" />
                <span>Price changes apply to new payments only. Payments already opened keep their amount.</span>
              </p>
            ) : (
              <>
                {networksSection}
                <PayoutRecap value={form.payout} onChange={(next) => set('payout', next)} data={payoutData} error={show} onTouch={(k) => touch(k)()} open={changing} onOpenChange={setChanging} hidden={hidden} />
                {offerLink ? (
                  <div className="check-row">
                    <input id="also-link" type="checkbox" checked={alsoLink} onChange={(e) => setAlsoLink(e.target.checked)} aria-describedby="also-link-hint" />
                    <div>
                      <label htmlFor="also-link" className="check-title">
                        Also create a payment link
                      </label>
                      <p className="hint" id="also-link-hint">
                        A link with no expiry or limit, ready to share. You can add more links to this product later.
                      </p>
                    </div>
                  </div>
                ) : null}
              </>
            )}
          </>
        )}
      </form>
      <div className="preview-col">
        <CheckoutPreview model={model} testmode={testmode} accepted={settings.data?.accepted_assets} chains={payoutData.ready ? takenChains(form.payout) : undefined} />
      </div>
      {payoutConfirm ? (
        <PayoutConfirmDialog
          changes={payoutConfirm}
          onClose={() => setPayoutConfirm(null)}
          onConfirm={() => {
            setPayoutConfirm(null);
            proceed();
          }}
        />
      ) : null}
      {product && decision ? <LinksDecisionDialog product={product} count={decision.count} busy={save.isPending} onConfirm={(links) => save.mutate(links)} onClose={() => setDecision(null)} /> : null}
      {guard.confirm}
    </Dialog>
  );
}
