import { useMemo } from 'react';
import { Link as RouterLink } from 'react-router-dom';
import { ConfirmDialog } from '../components/Dialog';
import { Icon } from '../components/Icon';
import { NetworkLogo } from '../components/Logos';
import { SplitBar } from '../components/SplitBar';
import { FieldError } from '../components/ui';
import { networkName } from '../lib/format';
import { chainFamily } from '../lib/logos';
import { truncateMiddle } from '../lib/money';
import {
  addRow,
  addRowBlocker,
  copyRows,
  copySources,
  formSummary,
  linkPayoutSummary,
  modeChains,
  NONE_KEY,
  pickable,
  removeRow,
  rowKey,
  rowsKey,
  sharesOfItems,
  sharesOfRows,
  shownChains,
  sumKey,
  sumMessage,
  takeChain,
  totalBps,
  type PayoutChange,
  type PayoutForm,
  type PayoutRow,
} from '../lib/payout';
import { useSettings, useWallets } from '../lib/queries';
import { FULL } from '../lib/splits';
import type { LinkPayout, Wallet } from '../lib/types';
import { activeWallets } from '../lib/wallets';

/** A product that pays no network yet: said, not left blank. */
const NO_PAYOUT = 'No network takes payments yet.';

const REMAINDER_HELP = 'Order matters only for rounding: the first wallet also gets the remainder, at most 0.000004 USDC or USDT per payment.';

/** The id of a payout field, the same scheme as the other product fields. */
const payoutFieldId = (key: string) => `product-${key.replace(/\./g, '-')}`;

export interface PayoutData {
  wallets: Wallet[];
  /** The networks of the mode, as the stablecoin list names them. */
  chains: string[];
  ready: boolean;
  /** The wallets or the settings could not be loaded. */
  failed: boolean;
}

export function usePayoutData(): PayoutData {
  const wallets = useWallets();
  const settings = useSettings();
  const accepted = settings.data?.accepted_assets;
  const chains = useMemo(() => modeChains(accepted ?? []), [accepted]);
  return { wallets: wallets.data?.data ?? [], chains, ready: wallets.isSuccess && settings.isSuccess, failed: wallets.isError || settings.isError };
}

const walletOption = (w: Wallet) => `${w.label} · ${truncateMiddle(w.address, 4, 4)}`;

interface EditorProps {
  value: PayoutForm;
  onChange: (next: PayoutForm) => void;
  data: PayoutData;
  /** The message to show for a key, or undefined while it should stay quiet. */
  error: (key: string) => string | undefined;
  onTouch: (key: string) => void;
  /** Networks the product's stablecoin choice has Off: no payout to edit there. */
  hidden?: string[];
}

interface NetworkProps extends Pick<EditorProps, 'data' | 'error' | 'onTouch'> {
  chain: string;
  rows: PayoutRow[];
  setRows: (rows: PayoutRow[]) => void;
  /** Networks of the same family this one can copy, with their rows. */
  sources: { chain: string; rows: PayoutRow[] }[];
}

function NetworkEditor({ chain, rows, setRows, sources, data, error, onTouch }: NetworkProps) {
  const name = networkName(chain);
  const total = totalBps(rows);
  const blocker = addRowBlocker(rows, data.wallets, chain);
  const setRow = (i: number, patch: Partial<PayoutRow>) => setRows(rows.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  const sumError = error(sumKey(chain));
  // Our own sum check repeats the live line, so it asks for the fix; a server refusal is shown as sent.
  const sumHelp = !sumError ? undefined : sumError === sumMessage(total) ? 'Change the shares so they add up to exactly 100%.' : sumError;
  const hasWallet = activeWallets(data.wallets, chainFamily(chain)).length > 0;
  const copies = sources.length > 0 ? (
    <>
      {sources.map((s) => (
        <button key={s.chain} type="button" className="btn btn-small btn-ghost" onClick={() => setRows(copyRows(s.rows))}>
          Same as {networkName(s.chain)}
        </button>
      ))}
    </>
  ) : null;
  return (
    <fieldset className="plain stack-md payout-family" data-testid={`payout-${chain}`}>
      <legend className="with-logo">
        <NetworkLogo chain={chain} size={16} decorative tag={false} />
        <span>Payments on {name}</span>
      </legend>
      {rows.length === 0 ? (
        <div className="stack-sm">
          <p className="hint">This product does not take payments on {name}.</p>
          {hasWallet ? (
            <div className="actions">
              <button type="button" className="btn btn-small" onClick={() => setRows(takeChain(data.wallets, chain))}>
                <Icon name="plus" />
                Take payments on {name}
              </button>
              {copies}
            </div>
          ) : null}
        </div>
      ) : (
        <>
          {rows.length > 1 ? <p className="hint">{REMAINDER_HELP}</p> : null}
          <ol className="split-rows">
            {rows.map((r, i) => {
              const wKey = rowKey(chain, i, 'wallet');
              const sKey = rowKey(chain, i, 'share');
              const wErr = error(wKey);
              const sErr = error(sKey);
              const wId = payoutFieldId(wKey);
              const sId = payoutFieldId(sKey);
              return (
                <li key={r.key} className="split-row" data-testid="payout-row">
                  <span className="split-swatch" data-seg={Math.min(i + 1, 5)} aria-hidden="true" />
                  <div className="field split-wallet">
                    <label htmlFor={wId} className="sr-only">
                      {name} wallet {i + 1}
                    </label>
                    <select
                      id={wId}
                      className="select"
                      value={r.wallet_id}
                      disabled={!data.ready}
                      aria-invalid={wErr ? true : undefined}
                      aria-describedby={wErr ? `${wId}-error` : undefined}
                      onChange={(e) => setRow(i, { wallet_id: e.target.value })}
                    >
                      {r.wallet_id ? null : <option value="">Choose a wallet</option>}
                      {pickable(rows, i, data.wallets, chain).map((w) => (
                        <option key={w.id} value={w.id}>
                          {walletOption(w)}
                        </option>
                      ))}
                    </select>
                    <FieldError id={`${wId}-error`} message={wErr} />
                  </div>
                  <div className="field split-share">
                    <label htmlFor={sId} className="sr-only">
                      Share of {name} wallet {i + 1}
                    </label>
                    <div className="input-wrap">
                      <input
                        id={sId}
                        className="input input-amount"
                        inputMode="decimal"
                        autoComplete="off"
                        value={r.share}
                        aria-invalid={sErr ? true : undefined}
                        aria-describedby={sErr ? `${sId}-error` : undefined}
                        onChange={(e) => setRow(i, { share: e.target.value })}
                        onBlur={() => onTouch(sKey)}
                      />
                      <span className="unit" aria-hidden="true">
                        %
                      </span>
                    </div>
                    <FieldError id={`${sId}-error`} message={sErr} />
                  </div>
                  <div className="split-row-actions">
                    {rows.length > 1 ? (
                      <button type="button" className="btn btn-ghost btn-icon" aria-label={`Remove ${name} wallet ${i + 1}`} onClick={() => setRows(removeRow(rows, i))}>
                        <Icon name="trash" />
                      </button>
                    ) : null}
                  </div>
                </li>
              );
            })}
          </ol>
          <FieldError id={`${payoutFieldId(rowsKey(chain))}-error`} message={error(rowsKey(chain))} />
          {rows.length > 1 || total !== FULL ? (
            <p className="sum-line" data-complete={total === FULL || undefined} aria-live="polite" data-testid={`payout-sum-${chain}`}>
              <Icon name={total === FULL ? 'check' : 'info'} />
              <span>{sumMessage(total)}</span>
            </p>
          ) : null}
          <FieldError id={`${payoutFieldId(sumKey(chain))}-error`} message={sumHelp} />
          <SplitBar shares={sharesOfRows(rows, data.wallets)} testId={`payout-bar-${chain}`} />
          <div className="actions">
            <button type="button" className="btn btn-small" onClick={() => setRows(addRow(rows, data.wallets, chain))} disabled={Boolean(blocker)} aria-describedby={blocker ? `payout-${chain}-add-help` : undefined}>
              <Icon name="plus" />
              Add wallet to this split
            </button>
            {copies}
          </div>
          {blocker ? (
            <p className="hint" id={`payout-${chain}-add-help`}>
              {blocker}
            </p>
          ) : null}
        </>
      )}
    </fieldset>
  );
}

/** "Where the money goes": one section per network of the mode whose family the merchant has wallets on. */
export function PayoutEditor({ value, onChange, data, error, onTouch, hidden = [] }: EditorProps) {
  const all = data.ready ? shownChains(data.wallets, value, data.chains) : [];
  const chains = all.filter((c) => !hidden.includes(c));
  return (
    <section className="stack-md" aria-labelledby="payout-title" data-testid="payout-section">
      <div>
        <h3 className="section-title" id="payout-title" tabIndex={-1}>
          Where the money goes
        </h3>
        <p className="hint">Each payment of this product, from every link, goes straight to your wallets, shared this way on each network. Payers never see it.</p>
      </div>
      {data.ready && all.length === 0 ? (
        <div className="callout" role="note">
          <Icon name="info" />
          <p>
            You have no wallets yet, so this product has nowhere to pay. <RouterLink to="/settings/networks">Add a wallet</RouterLink> first.
          </p>
        </div>
      ) : null}
      <div id={payoutFieldId(NONE_KEY)} tabIndex={-1} className="stack-md">
        {chains.map((c) => (
          <NetworkEditor
            key={c}
            chain={c}
            rows={value[c] ?? []}
            setRows={(rows) => onChange({ ...value, [c]: rows })}
            sources={copySources(value, c, chains).map((s) => ({ chain: s, rows: value[s]! }))}
            data={data}
            error={error}
            onTouch={onTouch}
          />
        ))}
      </div>
      <FieldError id={`${payoutFieldId(NONE_KEY)}-error`} message={error(NONE_KEY)} />
    </section>
  );
}

/**
 * "Where the money goes" for a new product: each network and the wallets it pays, as the form stands
 * (the default wallet at 100% to begin with), and a button that opens the split editor in place.
 */
export function PayoutRecap({ value, onChange, data, error, onTouch, open, onOpenChange, hidden = [] }: EditorProps & { open: boolean; onOpenChange: (open: boolean) => void }) {
  const all = data.ready ? shownChains(data.wallets, value, data.chains) : [];
  const chains = all.filter((c) => !hidden.includes(c));
  // With no wallet there is nothing to recap: the editor says to add one.
  if (open || (data.ready && all.length === 0)) {
    return (
      <div className="stack-md" data-testid="payout-editor">
        <PayoutEditor value={value} onChange={onChange} data={data} error={error} onTouch={onTouch} hidden={hidden} />
        {all.length > 0 ? (
          <div>
            <button type="button" className="btn btn-small" onClick={() => onOpenChange(false)}>
              Done
            </button>
          </div>
        ) : null}
      </div>
    );
  }
  return (
    <section className="recap" aria-labelledby="payout-title" data-testid="payout-recap">
      <div className="recap-text stack-sm">
        <h3 className="section-title" id="payout-title" tabIndex={-1}>
          Where the money goes
        </h3>
        <ul className="flat-list">
          {chains.map((c) => (
            <li key={c} className="with-logo" data-testid="payout-recap-line">
              <NetworkLogo chain={c} size={16} decorative tag={false} />
              <span className="recap-line">{formSummary(value, [c], data.wallets) || `${networkName(c)}: not taken`}</span>
            </li>
          ))}
        </ul>
        <p className="hint">Payers never see it. Each payment goes straight to these wallets.</p>
      </div>
      <button type="button" className="btn btn-small" onClick={() => onOpenChange(true)} disabled={!data.ready}>
        Change
      </button>
    </section>
  );
}

/** The compact line under the drawer: what each network pays out, as it stands in the form. */
export function PayoutSummary({ value, data, hidden = [] }: { value: PayoutForm; data: PayoutData; hidden?: string[] }) {
  if (!data.ready) return null;
  const line = formSummary(value, shownChains(data.wallets, value, data.chains).filter((c) => !hidden.includes(c)), data.wallets);
  return (
    <p className="payout-summary small" data-testid="payout-summary" aria-live="polite">
      {line || NO_PAYOUT}
    </p>
  );
}

/** Before a saved product's payout changes: for each network that changes, where it pays now and where it will. */
export function PayoutConfirmDialog({ changes, onConfirm, onClose }: { changes: PayoutChange[]; onConfirm: () => void; onClose: () => void }) {
  return (
    <ConfirmDialog
      title="Change where this product pays?"
      confirmLabel="Change payout"
      onClose={onClose}
      onConfirm={onConfirm}
      body={
        <div className="stack-md" data-testid="payout-confirm">
          <p>New payments will go to the wallets below. Payments already opened keep their wallets.</p>
          {changes.map((c) => (
            <div key={c.chain} className="stack-sm" data-testid={`payout-confirm-${c.chain}`}>
              <p className="label">{networkName(c.chain)}</p>
              <p className="small">
                <span className="muted">Now: </span>
                {c.before.length > 0 ? c.before.join(' · ') : 'Takes no payments'}
              </p>
              <p className="small">
                <span className="muted">New: </span>
                <strong>{c.after.length > 0 ? c.after.join(' · ') : 'Takes no payments'}</strong>
              </p>
            </div>
          ))}
        </div>
      }
    />
  );
}

/** One short line with every network a product pays. */
export function PayoutLines({ payout }: { payout: LinkPayout | undefined }) {
  const line = linkPayoutSummary(payout);
  if (!line)
    return (
      <span className="payout-lines small muted" data-testid="product-payout-lines">
        {NO_PAYOUT}
      </span>
    );
  return (
    <span className="payout-lines small muted" data-testid="product-payout-lines">
      {line}
    </span>
  );
}

/** The networks a product takes payments on, as logos with their names for assistive tech. */
export function PayoutNetworks({ payout }: { payout: LinkPayout | undefined }) {
  const chains = Object.keys(payout ?? {}).filter((c) => (payout?.[c]?.length ?? 0) > 0);
  if (chains.length === 0) return <span className="small muted">No network</span>;
  return (
    <span className="net-logos" data-testid="product-networks" aria-label={`Pays on ${chains.map(networkName).join(' and ')}`} role="img">
      {chains.map((c) => (
        <NetworkLogo key={c} chain={c} size={16} decorative tag={false} />
      ))}
    </span>
  );
}

/** Product detail: who each network pays, in payout order. */
export function ProductPayoutCard({ payout, onEdit }: { payout: LinkPayout | undefined; onEdit?: () => void }) {
  const data = usePayoutData();
  const line = linkPayoutSummary(payout, data.wallets);
  const chains = modeChains([], payout);
  return (
    <section className="card stack-md" aria-labelledby="payout-card-title" data-testid="product-payout">
      <div className="row between">
        <div>
          <h2 className="heading" id="payout-card-title">
            Where the money goes
          </h2>
          <p className="small muted">Payments opened from now on, from every link of this product and from your server, are shared this way. Payments already opened keep the wallets they started with.</p>
        </div>
        {onEdit ? (
          <button type="button" className="btn btn-small" onClick={onEdit} aria-label="Edit where the money goes">
            <Icon name="edit" />
            Edit
          </button>
        ) : null}
      </div>
      {line ? (
        <p className="payout-summary small" data-testid="product-payout-summary">
          {line}
        </p>
      ) : null}
      <ul className="flat-list split-list">
        {chains.map((c) => {
          const items = payout?.[c] ?? [];
          return (
            <li key={c} className="stack-sm" data-testid={`product-payout-${c}`}>
              <p className="subheading with-logo split-title">
                <NetworkLogo chain={c} size={16} decorative tag={false} />
                <span>{networkName(c)}</span>
              </p>
              {items.length > 0 ? (
                <SplitBar shares={sharesOfItems(items, data.wallets)} />
              ) : (
                <p className="small muted">This product does not take payments on {networkName(c)}. Edit the product to add a wallet for it.</p>
              )}
            </li>
          );
        })}
      </ul>
    </section>
  );
}

/** The link drawer: where a link's payments go, read from its product. */
export function PayoutReadOnly({ payout, onEdit }: { payout: LinkPayout | undefined; onEdit?: () => void }) {
  const data = usePayoutData();
  const line = linkPayoutSummary(payout, data.wallets);
  return (
    <section className="recap" aria-labelledby="link-payout-title" data-testid="link-payout">
      <div className="recap-text">
        <p className="label" id="link-payout-title">
          Where the money goes
        </p>
        <p className="small">{line || 'This product does not take payments on any network yet.'}</p>
        <p className="small muted">Set on the product, for every link of it.</p>
      </div>
      {onEdit ? (
        <button type="button" className="btn btn-small btn-ghost" onClick={onEdit}>
          <Icon name="edit" />
          Edit product
        </button>
      ) : null}
    </section>
  );
}
