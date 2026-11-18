import { Icon } from '../components/Icon';
import { AssetOnNetwork } from '../components/Logos';
import { Mark } from '../components/Shell';
import { logoSrc } from '../lib/logo';
import { useMe } from '../lib/queries';
import type { PreviewModel } from '../lib/formFields';
import type { AcceptedAsset } from '../lib/types';

/** A scaled copy of the hosted link page, from what the drawer holds so far. */
export function CheckoutPreview({ model, testmode, accepted = [], chains }: { model: PreviewModel; testmode: boolean; accepted?: AcceptedAsset[]; chains?: string[] }) {
  const merchant = useMe().data?.merchant;
  const offered = accepted.filter((a) => a.enabled && (!chains || chains.includes(a.chain)));
  return (
    <section className="preview" aria-label="Preview of the payment page">
      <p className="label">What the payer sees</p>
      <div className="preview-card stack-md" data-testid="link-preview" aria-hidden="true">
        {testmode ? (
          <p className="note">
            <Icon name="info" />
            <span>Test mode. Payments use test networks and have no value.</span>
          </p>
        ) : null}
        <div className="stack-sm preview-hero">
          {merchant?.logo_url ? (
            <span className="preview-merchant-logo">
              <img src={logoSrc(merchant.logo_url)} alt={merchant.name} width={160} height={40} data-testid="preview-logo" />
            </span>
          ) : null}
          <p className="display" data-testid="preview-name">
            {model.name}
          </p>
          {model.description ? (
            <p className="small muted preview-description" data-testid="preview-description">
              {model.description}
            </p>
          ) : null}
          {model.price_type === 'fixed' ? (
            <>
              <p className="amount-hero" data-testid="preview-price">
                {model.price ?? '0.00 USD'}
              </p>
              <p className="small muted">{model.hint}</p>
            </>
          ) : null}
        </div>
        {model.price_type === 'fixed' ? null : (
          <div className="stack-sm">
            <p className="label">Amount</p>
            <div className="fake-input">
              <span>{model.placeholder}</span>
              <span className="unit">USD</span>
            </div>
            <p className="small muted" data-testid="preview-hint">
              {model.hint}
            </p>
            {model.suggested.length > 0 ? (
              <div className="chips" data-testid="preview-chips">
                {model.suggested.map((s) => (
                  <span key={s} className="btn btn-small mono">
                    {s} USD
                  </span>
                ))}
              </div>
            ) : null}
          </div>
        )}
        <div className="fake-btn">
          Continue to payment
          <Icon name="arrow" />
        </div>
        {offered.length > 0 ? (
          <div className="stack-sm" data-testid="preview-assets">
            <p className="small muted">On the next step they choose from:</p>
            <ul className="preview-assets">
              {offered.map((a) => (
                <li key={`${a.chain}:${a.asset}`} className="small">
                  <AssetOnNetwork asset={a.asset} chain={a.chain} size={16} />
                </li>
              ))}
            </ul>
          </div>
        ) : null}
      </div>
      <div className="preview-footer" aria-hidden="true">
        <Mark size={20} />
        <span>Paid with Prismaport</span>
      </div>
    </section>
  );
}
