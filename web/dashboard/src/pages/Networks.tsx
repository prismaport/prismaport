import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useEffect, useState } from 'react';
import { Link as RouterLink } from 'react-router-dom';
import { Icon } from '../components/Icon';
import { AssetOnNetwork, NetworkLogo } from '../components/Logos';
import { useToast } from '../components/Toast';
import { ErrorState, Loading } from '../components/ui';
import { api } from '../lib/endpoints';
import { errorMessage } from '../lib/errors';
import { feeTermsText, type FeeTerms } from '../lib/formFields';
import { networkName } from '../lib/format';
import { leftWithNothing, productsText, tickedOn, usedBy } from '../lib/networkAssets';
import { chainFamily, networkInfo } from '../lib/logos';
import { useMode } from '../lib/mode';
import { invalidateOnboarding, keys, useSettings, useWallets } from '../lib/queries';
import type { AcceptedAsset, Family, SettingsProductNetwork, Wallet } from '../lib/types';
import { activeWallets, availableFamilies, familyLabel, FAMILIES, isActive, MAX_ACTIVE_WALLETS } from '../lib/wallets';
import { WalletDrawer } from './WalletDrawer';
import { ArchiveDialog, WalletRow } from './Wallets';

export function feeLine(fee: FeeTerms): string {
  return `Prismaport fee ${feeTermsText(fee)} per payment, split on-chain in the same transaction.`;
}

/** What payers can send on one network. Saving keeps the other networks' choices as they are. */
function Stablecoins({ chain, all, nets }: { chain: string; all: AcceptedAsset[]; nets: SettingsProductNetwork[] }) {
  const { mode } = useMode();
  const qc = useQueryClient();
  const toast = useToast();
  const mine = all.filter((a) => a.chain === chain);
  const [draft, setDraft] = useState(mine);
  const saved = JSON.stringify(mine);
  useEffect(() => setDraft(JSON.parse(saved) as AcceptedAsset[]), [saved]);
  const dirty = JSON.stringify(draft) !== saved;
  const merged = all.map((a) => draft.find((d) => d.chain === a.chain && d.asset === a.asset) ?? a);
  // Payers need one stablecoin somewhere, not on every network.
  const none = merged.every((a) => !a.enabled);
  const name = networkName(chain);
  const networkOn = draft.some((a) => a.enabled);
  // Products that take something on this network today and would take nothing once the draft is saved.
  const left = leftWithNothing(nets, all, merged, chain);
  const turnedOff = mine.filter((a) => a.enabled && draft.find((d) => d.asset === a.asset)?.enabled === false).map((a) => a.asset);
  const save = useMutation({
    mutationFn: () => api.setAcceptedAssets(mode, merged),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: keys.settings(mode) });
      invalidateOnboarding(qc);
      toast.show('Accepted stablecoins saved');
    },
  });
  return (
    <div className="stack-md">
      <label className="check network-switch">
        <input type="checkbox" role="switch" aria-label={`Accept payments on ${name}`} checked={networkOn} onChange={(e) => setDraft(draft.map((x) => ({ ...x, enabled: e.target.checked })))} />
        <span>Accept payments on {name}</span>
        <span className="tag" data-on={networkOn || undefined}>{networkOn ? 'Accepting payments' : 'Off'}</span>
      </label>
      <fieldset className="plain">
        <legend className="section-title">Stablecoins payers can send</legend>
        <div className="stack-sm">
          {draft.map((a, i) => {
            const used = usedBy(nets, chain, a.asset);
            const ticked = tickedOn(nets, chain, a.asset);
            return (
              <div key={a.asset} className="check asset-check">
                <input id={`stable-${chain}-${a.asset}`} type="checkbox" checked={a.enabled} aria-describedby={`used-${chain}-${a.asset}`} onChange={(e) => setDraft(draft.map((x, j) => (j === i ? { ...x, enabled: e.target.checked } : x)))} />
                <span className="stack-sm">
                  <label htmlFor={`stable-${chain}-${a.asset}`}>
                    <AssetOnNetwork asset={a.asset} chain={a.chain} />
                  </label>
                  <span className="small muted" id={`used-${chain}-${a.asset}`} data-testid={`used-${chain}-${a.asset}`}>
                    {a.enabled ? `Used by ${productsText(used)}` : ticked > 0 ? `Off · ticked on ${productsText(ticked)}` : 'Off'}
                  </span>
                </span>
              </div>
            );
          })}
        </div>
      </fieldset>
      {!networkOn ? (
        <p className="note" data-testid={`network-off-${chain}`}>
          <Icon name="info" />
          <span>
            <strong>{name} is off.</strong> No product takes payments on {name} until you turn it back on. Each product keeps its choice for {name}.
          </span>
        </p>
      ) : null}
      {left.length > 0 && turnedOff.length > 0 ? (
        <div className="callout" role="note" data-testid={`impact-${chain}`}>
          <Icon name="alert" />
          <div className="stack-sm">
            <p>
              <strong>
                If you save, {turnedOff.join(' and ')} {turnedOff.length === 1 ? 'stops' : 'stop'} on {name} for every product
              </strong>
            </p>
            <p>
              {productsText(left.length)} {left.length === 1 ? 'takes' : 'take'} only {turnedOff.join(' and ')} on {name}, so {left.length === 1 ? 'it stops' : 'they stop'} taking {name}:
            </p>
            <ul className="flat-list">
              {left.map((p) => (
                <li key={p.product_id}>
                  <RouterLink to={`/products/${p.product_id}`}>{p.product_name}</RouterLink>
                </li>
              ))}
            </ul>
            <p className="small muted">They keep {turnedOff.join(' and ')} ticked: turn {turnedOff.length === 1 ? 'it' : 'them'} back on here and they take {turnedOff.length === 1 ? 'it' : 'them'} again. Payments already opened are not affected.</p>
          </div>
        </div>
      ) : null}
      {none ? <p className="note" data-emphasis>Keep at least one stablecoin. Payers need one to pay.</p> : null}
      {save.isError ? <ErrorState error={save.error} /> : null}
      <div className="actions">
        <button type="button" className="btn btn-primary" disabled={!dirty || none || save.isPending} onClick={() => save.mutate()} aria-label={`Save ${networkName(chain)} stablecoins`}>
          Save stablecoins
        </button>
        {dirty ? (
          <button type="button" className="btn" onClick={() => setDraft(mine)}>
            Discard changes
          </button>
        ) : null}
      </div>
    </div>
  );
}

interface CardProps {
  chain: string;
  accepted: AcceptedAsset[];
  nets: SettingsProductNetwork[];
  wallets: Wallet[];
  showArchived: boolean;
  onAdd: (f: Family) => void;
  onArchive: (w: Wallet) => void;
  onMakeDefault: (w: Wallet) => void;
  busy: boolean;
}

function NetworkCard({ chain, accepted, nets, wallets, showArchived, onAdd, onArchive, onMakeDefault, busy }: CardProps) {
  const family = chainFamily(chain);
  const name = networkName(chain);
  const testnet = networkInfo(chain).testnet;
  const mine = wallets.filter((w) => w.family === family);
  const active = activeWallets(mine, family);
  const shown = showArchived ? [...active, ...mine.filter((w) => !isActive(w))] : active;
  const full = active.length >= MAX_ACTIVE_WALLETS;
  // The default is the wallet the API marks or, from an older API, the oldest: activeWallets puts it first.
  const marked = (w: Wallet): Wallet => (isActive(w) ? { ...w, default: active[0]?.id === w.id } : w);
  return (
    <section className="card stack-md" aria-labelledby={`network-${chain}-title`} data-testid={`network-${chain}`}>
      <h2 className="heading with-logo" id={`network-${chain}-title`}>
        <NetworkLogo chain={chain} size={20} decorative tag={false} />
        <span>{name}</span>
        <span className={testnet ? 'tag' : 'tag tag-live'}>{testnet ? 'Testnet' : 'Mainnet'}</span>
      </h2>
      <Stablecoins chain={chain} all={accepted} nets={nets} />
      <div className="stack-md" data-testid={`wallets-${family}`}>
        <div className="row between">
          <div>
            <h3 className="section-title">Wallets</h3>
            <p className="small muted">
              {active.length} of {MAX_ACTIVE_WALLETS} active wallets. New products pay to the default wallet.
            </p>
          </div>
          <button type="button" className="btn btn-small" onClick={() => onAdd(family)} disabled={full}>
            <Icon name="plus" />
            Add wallet on {name}
          </button>
        </div>
        {full ? (
          <p className="note">
            You have {MAX_ACTIVE_WALLETS} active {familyLabel(family)} wallets, the most there can be. Archive one to add another.
          </p>
        ) : null}
        {shown.length === 0 ? (
          <p className="muted">
            No {name} wallets yet. Payments on {name} are offered once you add one.
          </p>
        ) : (
          <ul className="flat-list wallet-list">
            {shown.map((w) => (
              <WalletRow key={w.id} wallet={marked(w)} onArchive={() => onArchive(w)} onMakeDefault={() => onMakeDefault(w)} busy={busy} />
            ))}
          </ul>
        )}
      </div>
    </section>
  );
}

/** A network the mode does not offer yet: shown so merchants know it is coming, but not editable. */
function ComingSoon({ family, wallets, showArchived, onArchive, onMakeDefault, busy }: { family: Family } & Pick<CardProps, 'wallets' | 'showArchived' | 'onArchive' | 'onMakeDefault' | 'busy'>) {
  const name = familyLabel(family);
  const mine = wallets.filter((w) => w.family === family);
  const active = activeWallets(mine, family);
  const shown = showArchived ? [...active, ...mine.filter((w) => !isActive(w))] : active;
  return (
    <section className="card stack-md" aria-labelledby={`network-${family}-soon`} aria-disabled="true" data-testid={`network-soon-${family}`}>
      <h2 className="heading with-logo" id={`network-${family}-soon`}>
        <NetworkLogo chain={family === 'solana' ? 'solana' : 'base'} size={20} decorative tag={false} />
        <span>{name}</span>
        <span className="tag">Coming soon</span>
      </h2>
      <p className="small muted">USDC and USDT on {name} are on the way. You will turn them on here.</p>
      {shown.length > 0 ? (
        <div className="stack-sm" data-testid={`wallets-${family}`}>
          <p className="small muted">These wallets receive nothing until {name} opens.</p>
          <ul className="flat-list wallet-list">
            {shown.map((w) => (
              <WalletRow key={w.id} wallet={w} onArchive={() => onArchive(w)} onMakeDefault={() => onMakeDefault(w)} busy={busy} />
            ))}
          </ul>
        </div>
      ) : null}
    </section>
  );
}

/** One card per network of the mode: the stablecoins payers can send on it and the wallets that receive them. */
export function NetworksTab() {
  const { mode } = useMode();
  const qc = useQueryClient();
  const toast = useToast();
  const wallets = useWallets();
  const settings = useSettings();
  const [adding, setAdding] = useState<Family | null>(null);
  const [archiving, setArchiving] = useState<Wallet | null>(null);
  const [showArchived, setShowArchived] = useState(false);
  const [problem, setProblem] = useState<string | null>(null);
  const makeDefault = useMutation({
    mutationFn: (w: Wallet) => api.makeDefaultWallet(mode, w.id),
    onSuccess: (_r, w) => {
      setProblem(null);
      void qc.invalidateQueries({ queryKey: keys.wallets(mode) });
      invalidateOnboarding(qc);
      toast.show(`${w.label} is now the default`);
    },
    onError: (e) => setProblem(errorMessage(e)),
  });

  if (wallets.isPending || settings.isPending) return <Loading what="networks" />;
  if (wallets.isError) return <ErrorState error={wallets.error} onRetry={() => wallets.refetch()} />;
  if (settings.isError) return <ErrorState error={settings.error} onRetry={() => settings.refetch()} />;
  const all = wallets.data.data;
  const accepted = settings.data.accepted_assets;
  const chains = [...new Set(accepted.map((a) => a.chain))];
  const available = availableFamilies(accepted);
  const soon = FAMILIES.filter((f) => !available.includes(f));
  const archivedCount = all.filter((w) => !isActive(w)).length;

  return (
    <div className="stack">
      <div className="row between">
        <p className="muted measure">Every payment goes straight to your wallets, shared the way each product says. We never hold it.</p>
        {archivedCount > 0 ? (
          <label className="check">
            <input type="checkbox" checked={showArchived} onChange={(e) => setShowArchived(e.target.checked)} />
            <span>Show archived wallets ({archivedCount})</span>
          </label>
        ) : null}
      </div>
      {problem ? (
        <div className="callout error-summary" role="alert">
          <Icon name="alert" />
          <p>{problem}</p>
        </div>
      ) : null}
      {chains.map((c) => (
        <NetworkCard
          key={c}
          chain={c}
          accepted={accepted}
          nets={settings.data.product_networks ?? []}
          wallets={all}
          showArchived={showArchived}
          onAdd={setAdding}
          onArchive={setArchiving}
          onMakeDefault={(w) => makeDefault.mutate(w)}
          busy={makeDefault.isPending}
        />
      ))}
      {soon.map((f) => (
        <ComingSoon key={f} family={f} wallets={all} showArchived={showArchived} onArchive={setArchiving} onMakeDefault={(w) => makeDefault.mutate(w)} busy={makeDefault.isPending} />
      ))}
      <p className="small muted">A stablecoin Prismaport adds later starts off on your account. Turn it on here to offer it; products set to All on that network take it at once.</p>
      <p className="small muted">{feeLine(settings.data.fee)}</p>
      {adding ? <WalletDrawer family={adding} onClose={() => setAdding(null)} /> : null}
      {archiving ? <ArchiveDialog wallet={archiving} onClose={() => setArchiving(null)} /> : null}
    </div>
  );
}
