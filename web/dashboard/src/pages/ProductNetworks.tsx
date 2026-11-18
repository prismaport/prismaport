import { Link as RouterLink } from 'react-router-dom';
import { Icon } from '../components/Icon';
import { AssetOnNetwork, NetworkLogo, TokenLogo } from '../components/Logos';
import { FieldError } from '../components/ui';
import { networkName } from '../lib/format';
import { chainFamily } from '../lib/logos';
import { useSettings } from '../lib/queries';
import { choiceOf, effectiveAssets, networksOfProduct, effectiveMethods, enabledAssets, offInSettings, type NetworkChoice, type NetworksForm } from '../lib/networkAssets';
import { shownChains, type PayoutForm } from '../lib/payout';
import type { AcceptedAsset, Product, Wallet } from '../lib/types';
import { familyLabel, isActive } from '../lib/wallets';
import { productFieldId } from './ProductFields';

export const NO_NETWORK_LEFT = 'No network left. Payers could not pay this product: take at least one network.';

const MODES: { id: NetworkChoice['accept']; label: string }[] = [
  { id: 'all', label: 'All' },
  { id: 'some', label: 'Some' },
  { id: 'off', label: 'Off' },
];

interface Props {
  networks: NetworksForm;
  payout: PayoutForm;
  accepted: AcceptedAsset[];
  wallets: Wallet[];
  /** The networks of the mode, as Settings lists them. */
  chains: string[];
  ready: boolean;
  onChoose: (chain: string, accept: NetworkChoice['accept']) => void;
  onToggle: (chain: string, asset: string) => void;
  /** The message to show for a key, or undefined while it should stay quiet. */
  error: (key: string) => string | undefined;
}

function SettingsLink({ children }: { children: string }) {
  return <RouterLink to="/settings/networks">{children}</RouterLink>;
}

function NetworkChoiceEditor({ chain, networks, accepted, hasWallet, onChoose, onToggle, error }: Pick<Props, 'networks' | 'accepted' | 'onChoose' | 'onToggle' | 'error'> & { chain: string; hasWallet: boolean }) {
  const name = networkName(chain);
  const choice = choiceOf(networks, chain);
  const on = enabledAssets(accepted, chain);
  const off = offInSettings(choice, accepted, chain);
  const effective = effectiveAssets(choice, accepted, chain);
  const key = `networks.${chain}`;
  const message = error(key);
  const id = productFieldId(key);
  // A network taken with nothing a payer can choose: said, with the way out.
  const nothing = choice.accept !== 'off' && effective.length === 0 && (choice.accept === 'all' || choice.assets.length > 0);
  const shownAssets = choice.accept === 'some' ? [...on, ...off] : [];
  return (
    <section className="network-choice stack-md" data-testid={`networks-${chain}`} aria-labelledby={`${id}-title`}>
      <div className="row between">
        <h4 className="subheading with-logo" id={`${id}-title`}>
          <NetworkLogo chain={chain} size={16} decorative tag={false} />
          <span>{name}</span>
        </h4>
        <div className="seg" role="group" aria-label={`Stablecoins on ${name}`} id={id} tabIndex={-1}>
          {MODES.map((m) => (
            <button key={m.id} type="button" className="seg-btn" aria-pressed={choice.accept === m.id} disabled={m.id !== 'off' && !hasWallet} onClick={() => onChoose(chain, m.id)}>
              {m.label}
            </button>
          ))}
        </div>
      </div>
      {choice.accept === 'all' ? (
        <div className="stack-sm">
          {on.length > 0 ? (
            <ul className="chips flat-list" aria-label={`Stablecoins taken on ${name}`}>
              {on.map((a) => (
                <li key={a} className="chip with-logo">
                  <TokenLogo asset={a} size={16} decorative />
                  <span>{a}</span>
                </li>
              ))}
            </ul>
          ) : null}
          <p className="hint">Every stablecoin on in Settings for {name}, including ones you turn on later.</p>
        </div>
      ) : null}
      {choice.accept === 'some' ? (
        <div className="stack-sm">
          <div className="stack-sm" role="group" aria-label={`Stablecoins ticked on ${name}`}>
            {shownAssets.map((a) => {
              const disabled = off.includes(a);
              return (
                <label key={a} className="check">
                  <input type="checkbox" checked={choice.assets.includes(a)} disabled={disabled} onChange={() => onToggle(chain, a)} />
                  <span className="with-logo">
                    <TokenLogo asset={a} size={16} decorative />
                    <span>{a}</span>
                    {disabled ? <span className="tag">Off in Settings</span> : null}
                  </span>
                </label>
              );
            })}
          </div>
          {choice.assets.length === 0 ? <p className="note" data-emphasis role="status">{`Tick at least one stablecoin, or set ${name} to Off.`}</p> : null}
          <p className="hint">Only the ones ticked. A stablecoin you turn on later in Settings does not join this product.</p>
        </div>
      ) : null}
      {nothing ? (
        <div className="callout" role="note" data-testid={`networks-nothing-${chain}`}>
          <Icon name="alert" />
          <div>
            <p>
              <strong>This product takes nothing on {name} right now</strong>
            </p>
            <p className="small">
              {choice.accept === 'all' ? `No stablecoin is on in Settings for ${name}, so payers won't see it. ` : `${off.join(' and ')} ${off.length === 1 ? 'is' : 'are'} off in Settings, so payers won't see ${name} for this product. `}
              <SettingsLink>{choice.accept === 'all' ? 'Turn a stablecoin on in Settings' : `Turn ${off.join(' and ')} on in Settings`}</SettingsLink>
              {choice.accept === 'some' && on.length > 0 ? `, or tick ${on.join(' or ')}.` : '.'}
            </p>
          </div>
        </div>
      ) : null}
      {choice.accept === 'off' ? (
        <p className="hint">
          {hasWallet ? `This product doesn't take payments on ${name}. Payers won't see it.` : `Add a ${familyLabel(chainFamily(chain))} wallet in Settings to take payments on ${name}.`}
          {hasWallet ? null : (
            <>
              {' '}
              <SettingsLink>Open Networks</SettingsLink>
            </>
          )}
        </p>
      ) : null}
      <FieldError id={`${id}-error`} message={message} />
    </section>
  );
}

/** "Payers will see": what the form gives payers, from Settings and the choices together. */
function PayersWillSee({ networks, accepted, chains }: Pick<Props, 'networks' | 'accepted'> & { chains: string[] }) {
  const methods = effectiveMethods(networks, accepted, chains);
  return (
    <section className="stack-sm" aria-labelledby="payers-see-title" data-testid="payers-see">
      <p className="label" id="payers-see-title">
        Payers will see
      </p>
      {methods.length === 0 ? (
        <p className="note" data-emphasis role="status">
          <Icon name="alert" />
          <span>{NO_NETWORK_LEFT}</span>
        </p>
      ) : (
        <ul className="chips flat-list" aria-labelledby="payers-see-title">
          {methods.map((m) => (
            <li key={`${m.chain}:${m.asset}`} className="chip">
              <AssetOnNetwork asset={m.asset} chain={m.chain} size={16} />
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

/** "Networks and stablecoins": per network the product can take, All, Some or Off, and what payers will see. */
export function ProductNetworks({ networks, payout, accepted, wallets, chains, ready, onChoose, onToggle, error }: Props) {
  const shown = ready ? shownChains(wallets, payout, chains) : [];
  if (!ready || shown.length === 0) return null;
  const hasWallet = (c: string) => wallets.some((w) => w.family === chainFamily(c) && isActive(w));
  return (
    <section className="stack-md" aria-labelledby="networks-title" data-testid="product-networks-section">
      <div>
        <h3 className="section-title" id="networks-title" tabIndex={-1}>
          Networks and stablecoins
        </h3>
        <p className="hint">
          Choose, network by network, which stablecoins this product takes. You can choose among the stablecoins that are on in <SettingsLink>Settings › Networks</SettingsLink>.
        </p>
      </div>
      {shown.map((c) => (
        <NetworkChoiceEditor key={c} chain={c} networks={networks} accepted={accepted} hasWallet={hasWallet(c)} onChoose={onChoose} onToggle={onToggle} error={error} />
      ))}
      <PayersWillSee networks={networks} accepted={accepted} chains={shown} />
    </section>
  );
}

/** Why a product has nothing to pay with, from its choices and Settings. */
function nothingReason(p: Pick<Product, 'networks' | 'payout'>, accepted: AcceptedAsset[]): string {
  const nets = networksOfProduct(p);
  if (nets.length === 0) return 'no network is taken';
  return nets
    .map((n) => {
      const name = networkName(n.chain);
      if (n.accept === 'all') return `no stablecoin on ${name} is on in Settings`;
      const assets = n.assets ?? [];
      return `${assets.join(' and ')} on ${name} ${assets.length === 1 ? 'is' : 'are'} off in Settings`;
    })
    .join('; ');
}

/** The products list: per network the product takes, its logo and the stablecoins payers can choose, with All or Some. */
export function PayWith({ product }: { product: Product }) {
  const settings = useSettings();
  const methods = product.payment_methods;
  if (!methods) return null;
  const nets = networksOfProduct(product);
  if (methods.length === 0) {
    const reason = settings.isSuccess ? nothingReason(product, settings.data.accepted_assets) : null;
    return (
      <span className="chip chip-warn" data-testid="product-pay-with" role="note">
        <Icon name="alert" />
        <span>{reason ? `Nothing to pay with: ${reason}` : 'Nothing to pay with'}</span>
      </span>
    );
  }
  return (
    <ul className="pay-with flat-list" data-testid="product-pay-with" aria-label="Payers can pay with">
      {nets.map((n) => {
        const assets = methods.filter((m) => m.chain === n.chain).map((m) => m.asset);
        if (assets.length === 0) return null;
        return (
          <li key={n.chain} className="with-logo" data-testid={`pay-with-${n.chain}`}>
            <NetworkLogo chain={n.chain} size={16} tag={false} />
            <span className="logo-group">
              {assets.map((a) => (
                <TokenLogo key={a} asset={a} size={16} />
              ))}
            </span>
            <span className="small muted">{n.accept === 'all' ? 'All' : 'Some'}</span>
          </li>
        );
      })}
    </ul>
  );
}
