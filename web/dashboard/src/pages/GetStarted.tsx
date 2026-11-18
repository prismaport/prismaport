import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useEffect, useMemo, useRef, useState } from 'react';
import { Link as RouterLink, Navigate } from 'react-router-dom';
import { Icon } from '../components/Icon';
import { FamilyLogos, AssetOnNetwork } from '../components/Logos';
import { useToast } from '../components/Toast';
import { CopyField, ErrorState, Loading, PageHead, ShortValue } from '../components/ui';
import { newIdempotencyKey } from '../lib/api';
import { api } from '../lib/endpoints';
import { errorMessage } from '../lib/errors';
import { hasMoreOptions } from '../lib/formFields';
import { chainFamily } from '../lib/logos';
import { useMode } from '../lib/mode';
import { prefillPayout, validatePayout } from '../lib/payout';
import { createProductBody, emptyProductForm, productPreview, recapLine, stepOfField, takenChains, validateProduct, type ProductFormState } from '../lib/productForm';
import { invalidateCatalog, invalidateOnboarding, keys, markStarted, useMe, useOnboarding, useSettings, useWallets } from '../lib/queries';
import type { AcceptedAsset, Family, Link, Mode, Wallet } from '../lib/types';
import { activeWallets, defaultWallet, familyLabel, FAMILIES } from '../lib/wallets';
import { CheckoutPreview } from './CheckoutPreview';
import { FormErrors } from './FormParts';
import { useProductSavedToast } from './ProductActions';
import type { ProductSaved } from './ProductDrawer';
import { ProductDetailsFields } from './ProductFields';
import { PayoutRecap, usePayoutData } from './ProductPayout';
import { useWalletForm } from './WalletDrawer';

type Step = 1 | 2 | 3 | 4;

const LABELS: Record<Exclude<Step, 4>, string> = { 1: 'Choose where to start', 2: 'Networks and wallet', 3: 'First product' };

function Stepper({ steps, current }: { steps: Exclude<Step, 4>[]; current: Step }) {
  return (
    <ol className="gs-steps" aria-label="Progress" data-testid="gs-steps">
      {steps.map((n, i) => (
        <li key={n} aria-current={current === n ? 'step' : undefined} data-done={current > n || undefined}>
          <span className="step-num" aria-hidden="true">
            {current > n ? <Icon name="check" size={14} /> : i + 1}
          </span>
          {LABELS[n]}
        </li>
      ))}
    </ol>
  );
}

/** Step 1, the very first time only: which mode to set up first. Neither locks the other. */
function StartStep({ onPick }: { onPick: (m: Mode) => void }) {
  const [start, setStart] = useState<Mode>('live');
  return (
    <section className="card stack-md" data-testid="step-start">
      <div className="kind-switch" role="radiogroup" aria-label="Where to start">
        <label className="kind-option">
          <input type="radio" name="start" value="live" checked={start === 'live'} onChange={() => setStart('live')} />
          <span className="kind-title">
            Live <span className="tag tag-live">Mainnet</span> <span className="tag">Default</span>
          </span>
          <span className="small muted">Base · real USDC and USDT</span>
          <span className="small muted">Customers pay you real stablecoins. Each payment goes straight to your wallet.</span>
        </label>
        <label className="kind-option">
          <input type="radio" name="start" value="test" checked={start === 'test'} onChange={() => setStart('test')} />
          <span className="kind-title">
            Test <span className="tag">Testnet</span>
          </span>
          <span className="small muted">Base Sepolia · no value</span>
          <span className="small muted">Try the whole flow without real money. A Simulate panel pays your links for you.</span>
        </label>
      </div>
      <p className="hint">You can use both. Live and Test keep their own wallets, products and links, and the switch at the top moves between them at any time.</p>
      <div className="actions">
        <button type="button" className="btn btn-primary" onClick={() => onPick(start)}>
          Continue in {start === 'live' ? 'Live' : 'Test'} mode
        </button>
      </div>
    </section>
  );
}

/** One new wallet on a network, inline: the same fields as Add wallet, and in Live mode the confirmation code. */
function AddWallet({ family, hasWallets, onDone, onCancel }: { family: Family; hasWallets: boolean; onDone: () => void; onCancel: () => void }) {
  const { mode } = useMode();
  const qc = useQueryClient();
  const toast = useToast();
  const form = useWalletForm({ family, fieldPrefix: `gs-${family}`, onSaved: onDone });
  const sample = useMutation({
    mutationFn: () => api.sampleWallet(mode, { family }),
    onSuccess: (w) => {
      void qc.invalidateQueries({ queryKey: keys.wallets(mode) });
      invalidateOnboarding(qc);
      toast.show(`${w.label} added`);
      onDone();
    },
  });
  return (
    <>
      <form className="stack-md" onSubmit={form.submit} noValidate data-testid={`add-wallet-${family}`}>
        <div className="row between">
          <h4 className="label">{hasWallets ? `Add another ${familyLabel(family)} wallet` : 'Receiving wallet'}</h4>
          {mode === 'test' && family === 'evm' ? (
            <button type="button" className="btn-link small" onClick={() => sample.mutate()} disabled={sample.isPending}>
              Use a sample test wallet
            </button>
          ) : null}
        </div>
        {sample.isError ? <ErrorState error={sample.error} /> : null}
        {form.fields}
        <div className="actions">
          <button type="submit" className="btn" disabled={form.pending}>
            {form.pending ? <span className="spinner" aria-hidden="true" /> : null}
            Add wallet
          </button>
          {hasWallets ? (
            <button type="button" className="btn btn-ghost" onClick={onCancel} disabled={form.pending}>
              Cancel
            </button>
          ) : null}
        </div>
      </form>
      {form.dialog}
    </>
  );
}

function WalletRow({ wallet, isDefault, canChange }: { wallet: Wallet; isDefault: boolean; canChange: boolean }) {
  const { mode } = useMode();
  const qc = useQueryClient();
  const make = useMutation({
    mutationFn: () => api.makeDefaultWallet(mode, wallet.id),
    onSuccess: () => {
      invalidateOnboarding(qc);
      return qc.invalidateQueries({ queryKey: keys.wallets(mode) });
    },
  });
  return (
    <li className="gs-wallet" data-testid="added-wallet">
      <div className="gs-wallet-main">
        <p className="subheading wallet-name">
          <span>{wallet.label}</span>
          {isDefault ? <span className="tag">Default</span> : null}
        </p>
        <p className="small muted">
          <ShortValue value={wallet.address} head={6} tail={4} />
        </p>
      </div>
      {isDefault ? (
        <span className="small muted">New products pay here</span>
      ) : canChange ? (
        <button type="button" className="btn btn-small btn-ghost" onClick={() => make.mutate()} disabled={make.isPending} aria-label={`Make ${wallet.label} the default wallet`}>
          Make default
        </button>
      ) : null}
    </li>
  );
}

interface CardProps {
  family: Family;
  items: AcceptedAsset[];
  onToggle: (a: AcceptedAsset, enabled: boolean) => void;
  wallets: Wallet[];
  /** Opens the new-wallet form at once, for the first network of the list. */
  first: boolean;
}

/** One network: the stablecoins payers can send on it and the wallets that receive them. */
function NetworkCard({ family, items, onToggle, wallets, first }: CardProps) {
  const { mode } = useMode();
  const mine = activeWallets(wallets, family);
  const def = defaultWallet(wallets, family);
  const [adding, setAdding] = useState(mine.length === 0 && first);
  const name = familyLabel(family);
  return (
    <section className="card stack-md gs-network" aria-label={name} data-testid={`network-${family}`}>
      <div className="row between">
        <h3 className="heading with-logo">
          <FamilyLogos family={family} accepted={items} size={24} />
          {name}
        </h3>
        <span className={mode === 'live' ? 'tag tag-live' : 'tag'}>{mode === 'live' ? 'Mainnet' : 'Testnet'}</span>
      </div>
      <fieldset className="plain stack-sm">
        <legend className="label">Stablecoins payers can send</legend>
        <div className="actions">
          {items.map((a) => (
            <label key={`${a.chain}:${a.asset}`} className="check">
              <input type="checkbox" checked={a.enabled} onChange={(e) => onToggle(a, e.target.checked)} />
              <AssetOnNetwork asset={a.asset} chain={a.chain} />
            </label>
          ))}
        </div>
      </fieldset>
      {mine.length > 0 ? (
        <div className="stack-sm">
          <h4 className="label">Wallets on {name}</h4>
          <ul className="flat-list" aria-label={`Your ${name} wallets`} data-testid="added-wallets">
            {mine.map((w) => (
              <WalletRow key={w.id} wallet={w} isDefault={w.id === def?.id} canChange={mine.length > 1} />
            ))}
          </ul>
          {mine.length > 1 ? <p className="hint">A product can split its payments between these wallets. You set the shares on the product.</p> : null}
          {adding ? null : (
            <div>
              <button type="button" className="btn" onClick={() => setAdding(true)}>
                <Icon name="plus" />
                Add another {name} wallet
              </button>
            </div>
          )}
        </div>
      ) : null}
      {mine.length === 0 && !adding ? (
        <div>
          <button type="button" className="btn" onClick={() => setAdding(true)}>
            <Icon name="wallet" />
            Add a {name} wallet
          </button>
        </div>
      ) : null}
      {adding ? <AddWallet family={family} hasWallets={mine.length > 0} onDone={() => setAdding(false)} onCancel={() => setAdding(false)} /> : null}
    </section>
  );
}

function ComingSoon({ family }: { family: Family }) {
  const name = familyLabel(family);
  return (
    <section className="card stack-sm gs-network" aria-disabled="true" aria-label={name} data-testid={`network-${family}-off`}>
      <div className="row between">
        <h3 className="heading with-logo">
          <FamilyLogos family={family} accepted={[]} size={24} />
          {name}
        </h3>
        <span className="tag">Coming soon</span>
      </div>
      <p className="hint">USDC and USDT on {name} are on the way. You will turn them on here.</p>
    </section>
  );
}

/** Step 2: the stablecoins per network and the wallets that receive them. */
function NetworksStep({ onBack, onNext }: { onBack?: () => void; onNext: () => void }) {
  const { mode } = useMode();
  const qc = useQueryClient();
  const settings = useSettings();
  const wallets = useWallets();
  const [draft, setDraft] = useState<AcceptedAsset[] | null>(null);
  const [problem, setProblem] = useState<string | null>(null);
  const saved = settings.data?.accepted_assets;
  const items = draft ?? saved ?? [];
  const save = useMutation({
    mutationFn: async () => {
      if (draft && JSON.stringify(draft) !== JSON.stringify(saved)) await api.setAcceptedAssets(mode, draft);
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: keys.settings(mode) });
      invalidateOnboarding(qc);
      onNext();
    },
  });
  if (settings.isError || wallets.isError) return <ErrorState error={settings.error ?? wallets.error} onRetry={() => void Promise.all([settings.refetch(), wallets.refetch()])} />;
  if (!saved || !wallets.data) return <Loading what="your networks" />;
  const all = wallets.data.data;
  const firstFamily = FAMILIES.find((f) => items.some((a) => chainFamily(a.chain) === f));

  const next = () => {
    if (!items.some((a) => a.enabled)) return setProblem('Keep at least one stablecoin. Payers need one to pay.');
    if (!all.some((w) => !w.archived_at)) return setProblem('Add a wallet first, so payments have somewhere to go.');
    setProblem(null);
    save.mutate();
  };

  return (
    <section className="stack-md" data-testid="step-networks">
      <div>
        <h2 className="heading">Where do you want to get paid?</h2>
        <p className="hint">Choose what payers can send on each network and the wallet that receives it. The same cards live in Settings.</p>
      </div>
      {FAMILIES.map((f) => {
        const mine = items.filter((a) => chainFamily(a.chain) === f);
        return mine.length > 0 ? (
          <NetworkCard
            key={f}
            family={f}
            items={mine}
            wallets={all}
            first={f === firstFamily}
            onToggle={(a, enabled) => {
              setProblem(null);
              setDraft(items.map((x) => (x.chain === a.chain && x.asset === a.asset ? { ...x, enabled } : x)));
            }}
          />
        ) : (
          <ComingSoon key={f} family={f} />
        );
      })}
      {problem ? (
        <p className="error" role="alert">
          <Icon name="alert" />
          <span>{problem}</span>
        </p>
      ) : null}
      {save.isError ? <ErrorState error={save.error} /> : null}
      <div className="actions">
        {onBack ? (
          <button type="button" className="btn" onClick={onBack} disabled={save.isPending}>
            Back
          </button>
        ) : null}
        <button type="button" className="btn btn-primary" onClick={next} disabled={save.isPending}>
          Continue
        </button>
      </div>
    </section>
  );
}

interface Done {
  summary: string;
  link: Link | null;
}

/** Step 3: the Add product form inline, with its checkout preview and where the money goes. */
function ProductStep({ onBack, onSaved }: { onBack: () => void; onSaved: (d: Done) => void }) {
  const { mode } = useMode();
  const qc = useQueryClient();
  const settings = useSettings();
  const payoutData = usePayoutData();
  const savedToast = useProductSavedToast();
  const fee = settings.data?.fee ?? null;
  const [form, setForm] = useState<ProductFormState>(() => emptyProductForm());
  const [alsoLink, setAlsoLink] = useState(true);
  const [moreOpen, setMoreOpen] = useState(false);
  // The recap of where the money goes, or its editor in place (Change, or a payout error on submit).
  const [changing, setChanging] = useState(false);
  const [submitted, setSubmitted] = useState(false);
  const [touched, setTouched] = useState<Set<string>>(() => new Set());
  const [serverError, setServerError] = useState<string | null>(null);
  const keysByBody = useRef(new Map<string, string>());

  // The product pays each network's default wallet until it is told otherwise.
  const prefilled = useRef(false);
  useEffect(() => {
    if (prefilled.current || !payoutData.ready) return;
    prefilled.current = true;
    setForm((f) => ({ ...f, payout: prefillPayout(payoutData.wallets, payoutData.chains) }));
  }, [payoutData.ready, payoutData.wallets, payoutData.chains]);

  const details = useMemo(() => validateProduct(form, { fee }), [form, fee]);
  const payoutCheck = useMemo(() => validatePayout(form.payout, payoutData.ready ? payoutData.wallets : undefined), [form.payout, payoutData.ready, payoutData.wallets]);
  const errors = useMemo(() => ({ ...details.errors, ...payoutCheck.errors }), [details.errors, payoutCheck.errors]);
  const input = details.input && payoutCheck.input && payoutData.ready ? { ...details.input, payout: payoutCheck.input } : null;
  const model = useMemo(() => productPreview(form), [form]);
  const show = (k: string) => (submitted || touched.has(k) ? errors[k] : undefined);
  const touch = (k: string) => () => setTouched((t) => (t.has(k) ? t : new Set(t).add(k)));
  const set = <K extends keyof ProductFormState>(k: K, v: ProductFormState[K]) => {
    setForm((f) => ({ ...f, [k]: v }));
    setServerError(null);
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
    mutationFn: async (): Promise<ProductSaved> => {
      if (!input) throw new Error('invalid');
      const body = createProductBody(input);
      const product = await api.createProduct(mode, body, keyFor(JSON.stringify(body)));
      if (!alsoLink) return { product, created: true, link: null, linkError: null };
      try {
        const link = await api.createLink(mode, { product_id: product.id }, keyFor(`link:${product.id}`));
        return { product, created: true, link, linkError: null };
      } catch (e) {
        return { product, created: true, link: null, linkError: e };
      }
    },
    onSuccess: (result) => {
      invalidateCatalog(qc);
      savedToast(result);
      onSaved({ summary: recapLine(model), link: result.link });
    },
    onError: (e) => setServerError(errorMessage(e)),
  });

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    setSubmitted(true);
    setServerError(null);
    if (!input) {
      if (hasMoreOptions(form)) setMoreOpen(true);
      if (Object.keys(errors).some((k) => stepOfField(k) === 'payout')) setChanging(true);
      return;
    }
    save.mutate();
  };
  const count = submitted ? Object.keys(errors).length : 0;

  return (
    <section className="card stack-md" data-testid="step-product">
      <div>
        <h2 className="heading">Create your first product</h2>
        <p className="hint">The same form as Products, Add product. It pays to the wallet from the last step.</p>
      </div>
      <div className="gs-product">
        <form id="gs-product-form" className="stack" onSubmit={submit} noValidate>
          <FormErrors count={count} serverError={serverError} />
          <ProductDetailsFields form={form} set={set} show={show} touch={touch} fee={fee} moreOpen={moreOpen} onToggleMore={() => setMoreOpen((o) => !o)} />
          <PayoutRecap value={form.payout} onChange={(payout) => set('payout', payout)} data={payoutData} error={show} onTouch={(k) => touch(k)()} open={changing} onOpenChange={setChanging} />
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
        </form>
        <div className="preview-col">
          <CheckoutPreview model={model} testmode={mode === 'test'} accepted={settings.data?.accepted_assets} chains={payoutData.ready ? takenChains(form.payout) : undefined} />
        </div>
      </div>
      <div className="actions">
        <button type="button" className="btn" onClick={onBack} disabled={save.isPending}>
          Back
        </button>
        <button type="submit" form="gs-product-form" className="btn btn-primary" disabled={save.isPending || !payoutData.ready}>
          {save.isPending ? <span className="spinner" aria-hidden="true" /> : null}
          Create product
        </button>
      </div>
    </section>
  );
}

function ReadyStep({ done }: { done: Done }) {
  const { mode } = useMode();
  const test = mode === 'test';
  const lead = `${done.summary} is ready${test ? ' in Test mode' : ''}. ${
    test ? 'Open the link and pay it with the Simulate panel. Nothing here moves real money.' : 'Share this link with whoever pays you, or put it on your site. Payments land in your wallet and show up under Payments.'
  }`;
  return (
    <section className="card stack-md" data-testid="step-ready">
      <div className="stack-sm">
        <h2 className="heading">You&apos;re ready{test ? ' to test' : ''}</h2>
        <p>{lead}</p>
      </div>
      {done.link ? <CopyField value={done.link.url} what="link" openLabel="Open the payment page" /> : <p className="hint">No link yet. Create one from the product page when you want to share it.</p>}
      <div className="actions">
        <RouterLink className="btn btn-primary" to="/links">
          Go to Payment links
        </RouterLink>
      </div>
      <p className="note">
        <Icon name="info" />
        <span>Get started leaves the menu now. {test ? 'When you want real payments, switch to Live at the top.' : 'Want to try a payment without real money? Switch to Test at the top.'}</span>
      </p>
    </section>
  );
}

/**
 * Live and Test each start here until the mode on screen has a product: where to start (the very
 * first time), the networks and wallets, a first product, then the link to share.
 */
export function GetStartedPage() {
  const { mode, setMode } = useMode();
  const me = useMe().data;
  const onboarding = useOnboarding();
  // The steps belong to the mode they were taken in: the switch at the top starts that mode's own.
  const [flow, setFlow] = useState<{ mode: Mode; step: Step; done: Done | null } | null>(null);
  const mine = flow && flow.mode === mode ? flow : null;
  // Where it opens is the server's call, so leaving and coming back resumes at the right step.
  const opens: Exclude<Step, 4> = onboarding.step === 'choose' ? 1 : onboarding.step === 'product' ? 3 : 2;
  // From then on the page keeps its own step: the refetch after a wallet is added must not move it on.
  useEffect(() => {
    if (!onboarding.loading && !mine) setFlow({ mode, step: opens, done: null });
  }, [onboarding.loading, mine, mode, opens]);
  if (onboarding.loading) return <Loading what="Get started" />;

  if (mode === 'live' && me?.merchant.live_enabled === false) {
    return (
      <>
        <PageHead title="Get started" lead="Try everything in Test mode first. Nothing there moves real money." />
        <div className="callout" role="note">
          <Icon name="info" />
          <div className="stack-sm">
            <p>Live payments open after a review. Until then, set up and try your account in Test mode.</p>
            <div>
              <button type="button" className="btn btn-small" onClick={() => setMode('test')}>
                Switch to Test mode
              </button>
            </div>
          </div>
        </div>
      </>
    );
  }
  // Once its first product exists the mode no longer needs Get started, except for the ready screen just reached.
  if (!onboarding.visible && mine?.step !== 4) return <Navigate to="/" replace />;

  const steps: Exclude<Step, 4>[] = onboarding.firstRun ? [1, 2, 3] : [2, 3];
  const step: Step = mine?.step ?? opens;
  const go = (s: Step, m: Mode = mode, done: Done | null = null) => setFlow({ mode: m, step: s, done });
  const title = onboarding.firstRun ? 'Get started' : `Set up ${mode === 'live' ? 'Live' : 'Test'} mode`;
  const lead = step === 4 ? undefined : onboarding.firstRun ? 'Three steps to your first payment link.' : `Two steps and ${mode === 'live' ? 'Live' : 'Test'} mode is ready too. It keeps its own wallets and products.`;

  return (
    <>
      <PageHead title={title} lead={lead} />
      {step === 4 ? null : <Stepper steps={steps} current={step} />}
      {step === 1 ? (
        <StartStep
          onPick={(m) => {
            if (me) markStarted(me.user.id);
            setMode(m);
            go(2, m);
          }}
        />
      ) : step === 2 ? (
        <NetworksStep key={mode} onBack={onboarding.firstRun ? () => go(1) : undefined} onNext={() => go(3)} />
      ) : step === 3 ? (
        <ProductStep key={mode} onBack={() => go(2)} onSaved={(done) => go(4, mode, done)} />
      ) : mine?.done ? (
        <ReadyStep done={mine.done} />
      ) : null}
    </>
  );
}
