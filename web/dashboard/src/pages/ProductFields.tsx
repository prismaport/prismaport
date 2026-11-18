import type { ReactNode } from 'react';
import { Field, FieldError } from '../components/ui';
import { feeTermsText, smallestPrice, type FeeTerms } from '../lib/formFields';
import { DESCRIPTION_MAX, type ProductFormState } from '../lib/productForm';
import { AmountInput, MetadataFields, MoreOptions } from './FormParts';

export const productFieldId = (key: string) => `product-${key.replace(/\./g, '-')}`;

interface Props {
  form: ProductFormState;
  set: <K extends keyof ProductFormState>(k: K, v: ProductFormState[K]) => void;
  show: (key: string) => string | undefined;
  touch: (key: string) => () => void;
  /** The price type of a saved product cannot change. */
  editing?: boolean;
  fee: FeeTerms | null;
  moreOpen: boolean;
  onToggleMore: () => void;
}

/** The details of a product (name, description, price, return address, metadata): the drawer's first step and Get started's third. */
export function ProductDetailsFields({ form, set, show, touch, editing = false, fee, moreOpen, onToggleMore }: Props): ReactNode {
  const id = productFieldId;
  return (
    <>
      <Field id={id('name')} label="Name" hint="Payers see it as the title of the payment page." error={show('name')}>
        {(p) => <input {...p} className="input" value={form.name} maxLength={240} onChange={(e) => set('name', e.target.value)} onBlur={touch('name')} data-autofocus />}
      </Field>

      <Field id={id('description')} label="Description" optional hint={`Shown to payers under the name. Plain text, up to ${DESCRIPTION_MAX} characters.`} error={show('description')}>
        {(p) => <textarea {...p} className="input textarea" rows={3} value={form.description} onChange={(e) => set('description', e.target.value)} onBlur={touch('description')} />}
      </Field>

      <fieldset className="plain" disabled={editing}>
        <legend>Price type</legend>
        {editing ? <p className="hint">The price type of a product cannot change after it is created. Add a new product for the other type.</p> : null}
        <div className="kind-switch">
          <label className="kind-option">
            <input type="radio" name="price_type" value="fixed" checked={form.price_type === 'fixed'} onChange={() => set('price_type', 'fixed')} />
            <span className="kind-title">Fixed price</span>
            <span className="small muted">One price, such as Founder plan at 25.00 USD.</span>
          </label>
          <label className="kind-option">
            <input type="radio" name="price_type" value="open" checked={form.price_type === 'open'} onChange={() => set('price_type', 'open')} />
            <span className="kind-title">Variable price</span>
            <span className="small muted">The amount is decided per link, by the payer, or by your server.</span>
          </label>
        </div>
      </fieldset>

      {form.price_type === 'fixed' ? (
        <Field id={id('amount')} label="Price" hint={fee ? `In USD, at most two decimals. Payers send exactly this amount in USDC or USDT. It must be at least ${smallestPrice(fee)} USD to settle after the Prismaport fee of ${feeTermsText(fee)}.` : 'In USD, at most two decimals. Payers send exactly this amount in USDC or USDT.'} error={show('amount')}>
          {(p) => <AmountInput p={p} placeholder="25.00" value={form.amount} onChange={(v) => set('amount', v)} onBlur={touch('amount')} />}
        </Field>
      ) : (
        <div className="stack-md">
          <div className="form-grid-2">
            <Field id={id('min_amount')} label="Minimum" hint="The smallest amount of a payment." error={show('min_amount')}>
              {(p) => <AmountInput p={p} placeholder="5.00" value={form.min_amount} onChange={(v) => set('min_amount', v)} onBlur={touch('min_amount')} />}
            </Field>
            <Field id={id('max_amount')} label="Maximum" optional hint="Leave empty for no maximum." error={show('max_amount')}>
              {(p) => <AmountInput p={p} placeholder="500.00" value={form.max_amount} onChange={(v) => set('max_amount', v)} onBlur={touch('max_amount')} />}
            </Field>
          </div>
          <fieldset className="plain">
            <legend>
              Suggested amounts <span className="optional muted">(optional, up to 3)</span>
            </legend>
            <div className="form-grid-3">
              {form.suggested.map((v, i) => {
                const k = `suggested.${i}`;
                const err = show(k);
                return (
                  <div key={k} className="field">
                    <label htmlFor={id(k)} className="sr-only">
                      Suggested amount {i + 1}
                    </label>
                    <input
                      id={id(k)}
                      className="input input-amount"
                      inputMode="decimal"
                      autoComplete="off"
                      placeholder={['10.00', '25.00', '50.00'][i]}
                      value={v}
                      aria-invalid={err ? true : undefined}
                      aria-describedby={err ? `${id(k)}-error` : undefined}
                      onChange={(e) => {
                        const next = [...form.suggested] as ProductFormState['suggested'];
                        next[i] = e.target.value;
                        set('suggested', next);
                      }}
                      onBlur={touch(k)}
                    />
                    <FieldError id={`${id(k)}-error`} message={err} />
                  </div>
                );
              })}
            </div>
            <p className="hint">Shown as buttons when the payer chooses the amount. Each must be within the minimum and maximum.</p>
          </fieldset>
        </div>
      )}

      <MoreOptions open={moreOpen} onToggle={onToggleMore} id="product-more" summary="Return address and metadata">
        <Field id={id('success_url')} label="Return address after paying" optional hint="Once paid, the payer can go back to this page, such as https://acme.com/thanks. A link can set its own." error={show('success_url')}>
          {(p) => <input {...p} className="input" type="url" inputMode="url" autoComplete="off" placeholder="https://" value={form.success_url} onChange={(e) => set('success_url', e.target.value)} onBlur={touch('success_url')} />}
        </Field>
        <MetadataFields
          rows={form.metadata}
          onChange={(rows) => set('metadata', rows)}
          show={show}
          touch={touch}
          fieldId={id}
          hint="Copied to every payment of this product and sent in webhooks. A link can add its own keys. Payers never see it."
        />
      </MoreOptions>
    </>
  );
}
