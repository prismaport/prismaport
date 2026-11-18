import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useEffect, useRef, useState } from 'react';
import { Link, NavLink } from 'react-router-dom';
import { CodeInput } from '../components/CodeInput';
import { ConfirmFields, useConfirmation } from '../components/Confirmation';
import { ConfirmDialog, Dialog } from '../components/Dialog';
import { Icon } from '../components/Icon';
import { NewPasswordField, useNewPassword } from '../components/NewPassword';
import { QrCode } from '../components/QrCode';
import { CreateCodesDialog, RecoveryCodesDialog } from '../components/RecoveryCodes';
import { useCancelRemoval } from '../components/Removal';
import { SecondFactorField, useSecondFactor } from '../components/SecondFactor';
import { useToast } from '../components/Toast';
import { CopyField, CopyValue, ErrorState, Field, PageHead, isApiError } from '../components/ui';
import { ApiError } from '../lib/api';
import { api } from '../lib/endpoints';
import { logoFileProblem, logoSizeProblem, logoSrc, readBase64 } from '../lib/logo';
import { useMode } from '../lib/mode';
import { keys, useMe } from '../lib/queries';
import { formatUtc, noteRecoverySignIn, recoverySignInNoted, REMOVAL_WAIT_HOURS } from '../lib/recovery';
import { NetworksTab } from './Networks';

export { feeLine } from './Networks';

/** The logo on the hosted checkout and payment link pages. It belongs to the merchant, not to a mode. */
function CheckoutLogo() {
  const me = useMe().data;
  const qc = useQueryClient();
  const toast = useToast();
  const input = useRef<HTMLInputElement>(null);
  const [problem, setProblem] = useState<string | null>(null);
  const [removing, setRemoving] = useState(false);
  const keep = (logo_url: string | null) => qc.setQueryData(keys.me, (m: typeof me) => (m ? { ...m, merchant: { ...m.merchant, logo_url } } : m));
  const upload = useMutation({
    mutationFn: async (file: File) => api.setLogo(await readBase64(file)),
    onSuccess: (r) => {
      keep(r.logo_url);
      toast.show('Checkout logo saved');
    },
    onError: (e) => setProblem(e.message),
  });
  const remove = useMutation({
    mutationFn: () => api.removeLogo(),
    onSuccess: () => {
      keep(null);
      setRemoving(false);
      toast.show('Checkout logo removed');
    },
  });
  const logo = me?.merchant.logo_url ?? null;
  return (
    <section className="card stack-md" aria-labelledby="logo-title">
      <div>
        <h2 className="heading" id="logo-title">
          Checkout logo
        </h2>
        <p className="small muted" id="logo-help">
          PNG, JPEG or WebP, up to 512 KB. Shown at the top of your checkout and payment link pages. We resize it to fit 256 x 256.
        </p>
      </div>
      {logo ? (
        <div className="logo-frame">
          <img src={logoSrc(logo)} alt="Your checkout logo" className="logo-img" />
        </div>
      ) : (
        <p className="small muted">No logo. Your checkout shows no business name or logo.</p>
      )}
      <input
        ref={input}
        id="logo-file"
        className="sr-only"
        type="file"
        tabIndex={-1}
        aria-label="Logo file"
        accept="image/png,image/jpeg,image/webp"
        aria-describedby="logo-help"
        disabled={upload.isPending}
        onChange={async (e) => {
          const file = e.target.files?.[0];
          e.target.value = '';
          if (!file) return;
          const p = logoFileProblem(file) ?? (await logoSizeProblem(file));
          setProblem(p);
          if (p) return;
          upload.mutate(file);
        }}
      />
      {problem ? (
        <p className="note" role="alert" data-emphasis>
          {problem}
        </p>
      ) : null}
      <div className="actions">
        <button type="button" className="btn btn-primary" disabled={upload.isPending} onClick={() => input.current?.click()}>
          {upload.isPending ? <span className="spinner" aria-hidden="true" /> : null}
          {logo ? 'Replace logo' : 'Upload logo'}
        </button>
        {logo ? (
          <button type="button" className="btn" disabled={upload.isPending} onClick={() => { remove.reset(); setRemoving(true); }}>
            Remove
          </button>
        ) : null}
      </div>
      {removing ? (
        <ConfirmDialog
          title="Remove your checkout logo"
          body="Your checkout and payment link pages will show no business name or logo."
          confirmLabel="Remove logo"
          busy={remove.isPending}
          error={remove.isError ? <ErrorState error={remove.error} /> : null}
          onConfirm={() => remove.mutate()}
          onClose={() => setRemoving(false)}
        />
      ) : null}
    </section>
  );
}

const CODE_RE = /^\d{6}$/;

/**
 * Turns 2FA on, or replaces the authenticator app when it is on. Turning it on asks for a code
 * emailed to the owner, and replacing it for the current code, so a stolen session cannot enroll its own
 * app; only then does the server hand out a new secret, which a first code from the app confirms.
 * Turning it on for the first time ends with the recovery codes. A recovery code can stand in for
 * the current code when the app is lost.
 */
function EnrollDialog({ replacing, onClose }: { replacing: boolean; onClose: () => void }) {
  const qc = useQueryClient();
  const toast = useToast();
  const [step, setStep] = useState<'confirm' | 'scan'>('confirm');
  // Turning 2FA on is confirmed with an emailed code; replacing the app, with the app you use now (or a recovery code).
  const emailed = useConfirmation('totp_enroll');
  const current = useSecondFactor('Enter the 6-digit code from the app you use now.');
  const [code, setCode] = useState('');
  const [codeErr, setCodeErr] = useState<string | null>(null);
  const [codes, setCodes] = useState<string[] | null>(null);
  const enroll = useMutation({
    mutationFn: () => api.totpEnroll(replacing ? current.payload('code') : { email_code: emailed.code.trim() }),
    onSuccess: () => {
      setStep('scan');
      setCode('');
      setCodeErr(null);
    },
    onError: (e) => {
      if (isApiError(e, 'totp_required')) current.refuse(e.message || 'That code is not valid. Enter the current code from your authenticator app.');
      else emailed.absorb(e);
    },
  });
  const verify = useMutation({
    mutationFn: () => api.totpVerify(code.trim()),
    onSuccess: (r) => {
      void qc.invalidateQueries({ queryKey: keys.me });
      noteRecoverySignIn(false);
      toast.show(replacing ? 'Authenticator app replaced' : 'Two-factor authentication is on');
      if (r?.recovery_codes?.length) setCodes(r.recovery_codes);
      else onClose();
    },
    onError: (e) => {
      if (isApiError(e, 'totp_required')) setCodeErr('That code did not match. Enter the current code from your authenticator app.');
    },
  });
  const startAgain = () => {
    verify.reset();
    enroll.reset();
    emailed.setCode('');
    setStep('confirm');
  };
  const busy = enroll.isPending || verify.isPending;
  const title = replacing ? 'Replace your authenticator app' : 'Turn on two-factor authentication';
  const fieldError = isApiError(enroll.error, 'invalid_code') || isApiError(enroll.error, 'confirmation_required') || isApiError(enroll.error, 'totp_required');

  if (codes) return <RecoveryCodesDialog codes={codes} onDone={onClose} />;
  return (
    <Dialog title={title} onClose={onClose} busy={busy}>
      {step === 'confirm' ? (
        <form
          className="form-grid"
          noValidate
          data-testid="enroll-confirm"
          onSubmit={(e) => {
            e.preventDefault();
            if (!(replacing ? current.check() : emailed.check())) return;
            enroll.mutate();
          }}
        >
          <p>
            {replacing
              ? 'Confirm with a current code from the app you use now, or a recovery code. That app keeps working until the new one is set up.'
              : 'Confirm with a code we email you. Then you scan a code with your authenticator app.'}
          </p>
          {replacing ? <SecondFactorField s={current} label="Current code" hint="From the authenticator app you use now." /> : <ConfirmFields s={emailed} idPrefix="enroll" what="Turning on two-factor authentication" inLive={false} />}
          {enroll.isError && !fieldError ? <ErrorState error={enroll.error} /> : null}
          <div className="dialog-foot">
            <button type="button" className="btn" onClick={onClose} disabled={busy}>
              Cancel
            </button>
            <button type="submit" className="btn btn-primary" disabled={busy}>
              Continue
            </button>
          </div>
        </form>
      ) : enroll.data ? (
        <form
          className="stack-md"
          noValidate
          data-testid="enroll-scan"
          onSubmit={(e) => {
            e.preventDefault();
            setCodeErr(null);
            if (!CODE_RE.test(code.trim())) return setCodeErr('Enter the 6-digit code from your authenticator app.');
            verify.mutate();
          }}
        >
          <p>
            {replacing ? 'Scan the code with your new authenticator app, or type the key, then enter the 6-digit code it shows.' : 'Scan the code with your authenticator app, or type the key, then enter the 6-digit code it shows.'}
          </p>
          <QrCode text={enroll.data.otpauth_url} label="QR code to add Prismaport to your authenticator app" />
          <CopyField value={enroll.data.secret} what="setup key" />
          <CodeInput
            label="Code"
            value={code}
            onChange={(v) => {
              setCode(v);
              setCodeErr(null);
            }}
            status={codeErr ? 'invalid' : 'idle'}
            message={codeErr ?? undefined}
            clearOnInvalid={false}
            autoFocus
          />
          {verify.isError && !isApiError(verify.error, 'totp_required') ? (
            <div className="stack-sm">
              <ErrorState error={verify.error} />
              <button type="button" className="btn btn-small" onClick={startAgain}>
                Start again
              </button>
            </div>
          ) : null}
          <div className="dialog-foot">
            <button type="submit" className="btn btn-primary" disabled={busy}>
              {replacing ? 'Use this app' : 'Turn on'}
            </button>
          </div>
        </form>
      ) : null}
    </Dialog>
  );
}

function TurnOffDialog({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient();
  const toast = useToast();
  const factor = useSecondFactor();
  const disable = useMutation({
    mutationFn: () => api.totpDisable(factor.payload('code')),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: keys.me });
      noteRecoverySignIn(false);
      toast.show('Two-factor authentication is off');
      onClose();
    },
    onError: (e) => {
      if (isApiError(e, 'totp_required')) factor.refuse(e.message || 'That code is not valid. Enter the current code from your authenticator app.');
    },
  });
  const fieldError = isApiError(disable.error, 'totp_required');
  return (
    <Dialog title="Turn off two-factor authentication" onClose={onClose} busy={disable.isPending} description="Confirm with a current code or a recovery code.">
      <form
        className="form-grid"
        noValidate
        onSubmit={(e) => {
          e.preventDefault();
          if (!factor.check()) return;
          disable.mutate();
        }}
      >
        <p className="callout">
          <Icon name="alert" />
          <span>Your password alone will sign in, and a code we email you will create live API keys and add or archive wallets. Your recovery codes stop working. We email you to confirm.</span>
        </p>
        <SecondFactorField s={factor} label="Code" autoFocus />
        {disable.isError && !fieldError ? <ErrorState error={disable.error} /> : null}
        <div className="dialog-foot">
          <button type="button" className="btn" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="btn btn-primary" disabled={disable.isPending}>
            Turn off
          </button>
        </div>
      </form>
    </Dialog>
  );
}

function TwoFactor() {
  const me = useMe().data;
  const cancel = useCancelRemoval();
  const [enrolling, setEnrolling] = useState<null | 'on' | 'replace'>(null);
  const [disabling, setDisabling] = useState(false);
  const [creating, setCreating] = useState(false);

  const on = Boolean(me?.user.totp_enabled);
  const left = me?.user.recovery_codes_left;
  const removalAt = me?.user.totp_removal_at;
  // A recovery code signed this tab in: the app may be lost, so replacing it comes first.
  const viaRecovery = on && !removalAt && recoverySignInNoted() && left !== undefined;
  const noCodes = on && !removalAt && left === 0;

  if (on && removalAt) {
    return (
      <section className="card stack-md" aria-labelledby="tfa-title">
        <div className="row between">
          <div>
            <h2 className="heading" id="tfa-title">
              Two-factor authentication
            </h2>
            <p className="small muted">On until {formatUtc(removalAt)}. A recovery request removes it then.</p>
          </div>
          <button type="button" className="btn btn-primary" disabled={cancel.isPending} onClick={() => cancel.mutate()}>
            Cancel removal
          </button>
        </div>
        <p className="callout">
          <Icon name="alert" />
          <span>Did you not ask for this? Cancel the removal and change your password. Someone may know your password and have access to your email.</span>
        </p>
        {cancel.isError ? <ErrorState error={cancel.error} /> : null}
      </section>
    );
  }

  return (
    <section className="card stack-md" aria-labelledby="tfa-title">
      <div className="row between">
        <div>
          <h2 className="heading" id="tfa-title">
            Two-factor authentication
          </h2>
          <p className="small muted">
            {on ? 'On. We ask for a code when you sign in, create a live API key, or add or archive a wallet.' : 'Off. Turn it on to protect sign-in, live API keys and wallets.'}
          </p>
        </div>
        {on ? (
          <div className="row">
            <button type="button" className={viaRecovery ? 'btn btn-primary' : 'btn'} onClick={() => setEnrolling('replace')}>
              Replace app
            </button>
            <button type="button" className="btn" onClick={() => setDisabling(true)}>
              Turn off
            </button>
          </div>
        ) : (
          <button type="button" className="btn btn-primary" onClick={() => setEnrolling('on')}>
            <Icon name="shield" />
            Turn on
          </button>
        )}
      </div>
      {viaRecovery ? (
        <p className="callout" data-testid="recovery-sign-in">
          <Icon name="alert" />
          <span>You signed in with a recovery code. {left} of 10 left. If you lost your authenticator app, replace it now.</span>
        </p>
      ) : null}
      {noCodes ? (
        <div className="callout" data-testid="no-recovery-codes">
          <Icon name="alert" />
          <div className="row between grow">
            <span>You have no recovery codes. If you lose your authenticator app, getting back in takes {REMOVAL_WAIT_HOURS} hours.</span>
            <button type="button" className="btn btn-primary" onClick={() => setCreating(true)}>
              Create recovery codes
            </button>
          </div>
        </div>
      ) : null}
      {on && left !== undefined && left > 0 ? (
        <>
          <hr className="divider" />
          <div className="row between">
            <div>
              <h3 className="subheading">Recovery codes</h3>
              <p className="small muted">{left} of 10 left. Each one works once, if you lose your authenticator app.</p>
            </div>
            <button type="button" className="btn" onClick={() => setCreating(true)}>
              Create new codes
            </button>
          </div>
        </>
      ) : null}
      {enrolling ? <EnrollDialog replacing={enrolling === 'replace'} onClose={() => setEnrolling(null)} /> : null}
      {disabling ? <TurnOffDialog onClose={() => setDisabling(false)} /> : null}
      {creating ? <CreateCodesDialog replacing={Boolean(left && left > 0)} onClose={() => setCreating(false)} /> : null}
    </section>
  );
}

/** Changes the password from inside the dashboard; every other device is signed out, this one stays. */
function ChangePasswordDialog({ onClose }: { onClose: () => void }) {
  const toast = useToast();
  const totpOn = Boolean(useMe().data?.user.totp_enabled);
  const [current, setCurrent] = useState('');
  const [currentErr, setCurrentErr] = useState<string | undefined>();
  const newPassword = useNewPassword();
  const [code, setCode] = useState('');
  const [codeErr, setCodeErr] = useState<string | undefined>();
  const change = useMutation({
    mutationFn: () => api.changePassword({ current_password: current, new_password: newPassword.password, ...(totpOn ? { totp: code.trim() } : {}) }),
    onSuccess: () => {
      toast.show('Password changed. Other devices were signed out.');
      onClose();
    },
    onError: (e) => {
      if (isApiError(e, 'invalid_password')) setCurrentErr(e.message || 'That password is not correct.');
      else if (isApiError(e, 'totp_required')) setCodeErr(e.message || 'That code is not valid. Enter the current code from your authenticator app.');
      else newPassword.refused(e);
    },
  });
  const handled = isApiError(change.error, 'invalid_password') || isApiError(change.error, 'totp_required') || (change.error instanceof ApiError && change.error.param === 'new_password');
  return (
    <Dialog title="Change password" description="Other devices are signed out. This one stays signed in." onClose={onClose} busy={change.isPending}>
      <form
        className="form-grid"
        noValidate
        data-testid="change-password"
        onSubmit={(e) => {
          e.preventDefault();
          const cur = current ? undefined : 'Enter your current password.';
          const cd = totpOn && !CODE_RE.test(code.trim()) ? 'Enter the 6-digit code from your authenticator app.' : undefined;
          setCurrentErr(cur);
          setCodeErr(cd);
          const fine = newPassword.validate();
          if (cur || cd || !fine) return;
          change.mutate();
        }}
      >
        <Field label="Current password" error={currentErr}>
          {(p) => <input {...p} className="input" type="password" autoComplete="current-password" value={current} onChange={(e) => { setCurrent(e.target.value); setCurrentErr(undefined); }} data-autofocus />}
        </Field>
        <NewPasswordField {...newPassword.field} label="New password" />
        {totpOn ? (
          <CodeInput
            label="Authenticator code"
            hint="Two-factor authentication is on for this account."
            value={code}
            onChange={(v) => {
              setCode(v);
              setCodeErr(undefined);
            }}
            status={codeErr ? 'invalid' : 'idle'}
            message={codeErr}
            clearOnInvalid={false}
          />
        ) : null}
        {change.isError && !handled ? <ErrorState error={change.error} /> : null}
        <div className="dialog-foot dialog-foot-spread">
          <Link className="small" to="/forgot-password" onClick={onClose}>
            Forgot your password?
          </Link>
          <div className="row">
            <button type="button" className="btn" onClick={onClose} disabled={change.isPending}>
              Cancel
            </button>
            <button type="submit" className="btn btn-primary" disabled={change.isPending}>
              Change password
            </button>
          </div>
        </div>
      </form>
    </Dialog>
  );
}

function PasswordCard() {
  const [changing, setChanging] = useState(false);
  return (
    <section className="card" aria-labelledby="password-title">
      <div className="row between">
        <div>
          <h2 className="heading" id="password-title">
            Password
          </h2>
          <p className="small muted">We ask for it when you sign in, and before you add a wallet or create a live API key.</p>
        </div>
        <button type="button" className="btn" onClick={() => setChanging(true)}>
          Change password
        </button>
      </div>
      {changing ? <ChangePasswordDialog onClose={() => setChanging(false)} /> : null}
    </section>
  );
}

export type SettingsTab = 'networks' | 'account';

function SettingsTabs() {
  return (
    <nav className="tabs" aria-label="Settings">
      <NavLink to="/settings/networks" className="tab">
        Networks
      </NavLink>
      <NavLink to="/settings/account" className="tab">
        Account
      </NavLink>
    </nav>
  );
}

function AccountTab() {
  const me = useMe().data;
  return (
    <div className="stack">
      <CheckoutLogo />
      <PasswordCard />
      <TwoFactor />
      <section className="card stack-md" aria-labelledby="account-title">
        <h2 className="heading" id="account-title">
          Account
        </h2>
        <dl className="details">
          <dt>Business</dt>
          <dd>{me?.merchant.name}</dd>
          <dt>Merchant ID</dt>
          <dd>{me ? <CopyValue value={me.merchant.id} what="merchant ID" head={8} tail={4} /> : null}</dd>
          <dt>Email</dt>
          <dd>{me?.user.email}</dd>
        </dl>
      </section>
    </div>
  );
}

export function SettingsPage({ tab }: { tab: SettingsTab }) {
  const { mode } = useMode();
  return (
    <>
      <PageHead title="Settings" lead={`Where payments go and what payers can send, in ${mode === 'test' ? 'Test' : 'Live'} mode.`} />
      <SettingsTabs />
      {tab === 'networks' ? <NetworksTab /> : <AccountTab />}
    </>
  );
}
