import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useState, type FormEvent, type ReactNode } from 'react';
import { CodeInput, type CodeStatus } from '../components/CodeInput';
import { ConfirmFields, useConfirmation } from '../components/Confirmation';
import { Dialog } from '../components/Dialog';
import { Icon } from '../components/Icon';
import { FamilyLogos } from '../components/Logos';
import { useRecoveryLock } from '../components/RecoveryLock';
import { useToast } from '../components/Toast';
import { Field } from '../components/ui';
import { api } from '../lib/endpoints';
import { errorMessage } from '../lib/errors';
import { useMode } from '../lib/mode';
import { invalidateOnboarding, keys, useSettings } from '../lib/queries';
import type { Family, Wallet } from '../lib/types';
import { CONFIRM_CHARS, confirmCaseNote, confirmTailError, availableFamilies, familyLabel, LABEL_MAX, tailMatches, walletAddressError, walletLabelError } from '../lib/wallets';

const ORDER = ['label', 'address', 'confirm'];

export interface WalletForm {
  /** The name, address and confirmation fields, the Live confirmation and a refusal; no network picker, no buttons. */
  fields: ReactNode;
  submit: (e?: FormEvent) => void;
  pending: boolean;
  /** The recovery-lock dialog, to render next to the form. */
  dialog: ReactNode;
}

interface FormOptions {
  family: Family;
  address?: string;
  /** Prefix of the field ids, so two forms on a page do not clash. */
  fieldPrefix?: string;
  onSaved: (w: Wallet) => void;
}

/** The form of one new wallet on one network, in a drawer or inline in Get started. */
export function useWalletForm({ family, address: initialAddress = '', fieldPrefix = 'wallet', onSaved }: FormOptions): WalletForm {
  const { mode } = useMode();
  const live = mode === 'live';
  const qc = useQueryClient();
  const toast = useToast();
  const confirmation = useConfirmation('wallet_create', mode);
  const lock = useRecoveryLock();
  const [label, setLabel] = useState('');
  const [address, setAddress] = useState(initialAddress);
  const [confirm, setConfirm] = useState('');
  const [submitted, setSubmitted] = useState(false);
  const [serverError, setServerError] = useState<string | null>(null);
  const name = familyLabel(family);
  const fid = (k: string) => `${fieldPrefix}-${k}`;

  const errors: Record<string, string | null> = {
    label: walletLabelError(label),
    address: walletAddressError(family, address),
    confirm: walletAddressError(family, address) ? null : confirmTailError(family, address, confirm),
  };
  const show = (k: string) => (submitted ? errors[k] ?? undefined : undefined);
  // The confirmation answers as soon as all its boxes are filled; a shorter one only after a submit.
  const addressOk = !errors.address;
  const full = confirm.length === CONFIRM_CHARS;
  const confirmStatus: CodeStatus = addressOk && full ? (tailMatches(family, address, confirm) ? 'confirmed' : 'invalid') : 'idle';
  const confirmMessage = confirmStatus === 'confirmed' ? `These match the last ${CONFIRM_CHARS} characters of the address.` : full ? errors.confirm : show('confirm');

  const save = useMutation({
    mutationFn: () => api.createWallet(mode, { label: label.trim(), family, address: address.trim(), ...(live ? confirmation.payload() : {}) }),
    onSuccess: (w) => {
      // In the list at once, so whatever opened this form can use it.
      qc.setQueryData<{ data: Wallet[] }>(keys.wallets(mode), (old) => (old ? { data: [...old.data, w] } : old));
      void qc.invalidateQueries({ queryKey: keys.wallets(mode) });
      invalidateOnboarding(qc);
      if (live) confirmation.done();
      toast.show(`${w.label} added`);
      onSaved(w);
    },
    onError: (e) => {
      if (lock.absorbs(e)) lock.onError(e);
      else if (!(live && confirmation.absorb(e))) setServerError(errorMessage(e));
    },
  });

  const submit = (e?: FormEvent) => {
    e?.preventDefault();
    setSubmitted(true);
    setServerError(null);
    // A lock after a lost-app removal is told before anything is checked or sent.
    if (lock.blocked()) return;
    const first = ORDER.find((k) => errors[k]);
    if (first) {
      if (live) confirmation.showMissing();
      return document.getElementById(fid(first))?.focus();
    }
    if (!live) return save.mutate();
    confirmation.run(
      () => save.mutate(),
      () => document.getElementById(`${fieldPrefix}-step-code`)?.focus(),
    );
  };

  const fields = (
    <>
      <Field id={fid('label')} label="Name" hint={`Only you see it, in your links and payments. At most ${LABEL_MAX} characters.`} error={show('label')}>
        {(p) => <input {...p} className="input" value={label} maxLength={LABEL_MAX + 20} autoComplete="off" onChange={(e) => setLabel(e.target.value)} data-autofocus />}
      </Field>
      <Field id={fid('address')} label={`${name} wallet address`} hint="Paste your wallet address. It cannot be edited later: to use another address, add a new wallet and archive this one." error={show('address')}>
        {(p) => <input {...p} className="input mono" autoComplete="off" spellCheck={false} value={address} onChange={(e) => setAddress(e.target.value)} />}
      </Field>
      <CodeInput
        id={fid('confirm')}
        mode="alphanumeric"
        length={CONFIRM_CHARS}
        caseInsensitive={family === 'evm'}
        label={`Confirm the last ${CONFIRM_CHARS} characters`}
        hint={`Read them from your wallet app, not from what you pasted, so a wrong paste is caught. ${confirmCaseNote(family)}`}
        value={confirm}
        onChange={setConfirm}
        status={confirmStatus}
        message={confirmMessage ?? undefined}
        tone={confirmStatus === 'idle' && confirmMessage ? 'error' : undefined}
        clearOnInvalid={false}
      />
      <p className="note">
        <Icon name="info" />
        <span>
          {live
            ? 'We email you whenever a Live wallet is added, so you know if anyone else did it. Adding a wallet changes no product: your default wallet on a network is where new products pay.'
            : 'Adding a wallet changes no product. Your default wallet on a network is where new products pay; choose the wallets and their shares on each product.'}
        </span>
      </p>
      {serverError ? (
        <div className="callout error-summary" role="alert">
          <Icon name="alert" />
          <p>{serverError}</p>
        </div>
      ) : null}
      {live ? <ConfirmFields s={confirmation} idPrefix={`${fieldPrefix}-step`} /> : null}
    </>
  );
  return { fields, submit, pending: save.isPending, dialog: lock.dialog };
}

export function WalletDrawer({ family: initialFamily, address: initialAddress = '', onClose, onSaved }: { family: Family; address?: string; onClose: () => void; onSaved?: (w: Wallet) => void }) {
  const settings = useSettings();
  const accepted = settings.data?.accepted_assets ?? [];
  // Only the families the settings offer a chain for; until they load, the one asked for.
  const families = settings.data ? availableFamilies(accepted) : [initialFamily];
  const [picked, setFamily] = useState<Family>(initialFamily);
  const family = families.includes(picked) ? picked : (families[0] ?? picked);
  const form = useWalletForm({
    family,
    address: initialAddress,
    onSaved: (w) => {
      onSaved?.(w);
      onClose();
    },
  });

  return (
    <>
      <Dialog
        variant="drawer"
        size="narrow"
        title="Add wallet"
        onClose={onClose}
        busy={form.pending}
        footer={
          <>
            <button type="button" className="btn" onClick={onClose} disabled={form.pending}>
              Cancel
            </button>
            <button type="submit" form="wallet-form" className="btn btn-primary" disabled={form.pending || families.length === 0}>
              {form.pending ? <span className="spinner" aria-hidden="true" /> : null}
              Add wallet
            </button>
          </>
        }
      >
        <form id="wallet-form" className="stack" onSubmit={form.submit} noValidate>
          {families.length === 0 ? (
            <p className="muted" data-testid="wallet-no-network">
              No network is open for wallets yet.
            </p>
          ) : null}
          {families.length > 1 ? (
          <fieldset className="plain">
            <legend>Network</legend>
            <div className="kind-switch">
              {families.map((f) => (
                <label key={f} className="kind-option">
                  <input type="radio" name="wallet-family" value={f} checked={family === f} onChange={() => setFamily(f)} />
                  <span className="kind-title with-logo">
                    <FamilyLogos family={f} accepted={accepted} size={16} />
                    <span>{familyLabel(f)}</span>
                  </span>
                </label>
              ))}
            </div>
          </fieldset>
          ) : null}
          {families.length > 0 ? form.fields : null}
        </form>
      </Dialog>
      {form.dialog}
    </>
  );
}
