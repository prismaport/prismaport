import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useEffect, useState, type FormEvent, type ReactNode } from 'react';
import { NavLink, Link as RouterLink, useParams, useSearchParams } from 'react-router-dom';
import { ConfirmDialog, Dialog } from '../components/Dialog';
import { Icon } from '../components/Icon';
import { useRecoveryLock } from '../components/RecoveryLock';
import { useLiveConfirmation } from '../components/Confirmation';
import { useToast } from '../components/Toast';
import { CopyField, ErrorState, Field, FieldError, Loading, PageHead, Pill, Segmented, ShortValue } from '../components/ui';
import { ALL_SCOPES, canRoll, describeScopes, isFullAccess, keyPill, lastUsedLabel, NO_ACCESS, ROLL_OVERLAPS, SCOPE_GROUPS, scopesFromAccess, type Access, type AccessMap } from '../lib/apiKeys';
import { api } from '../lib/endpoints';
import { formatDateTime } from '../lib/format';
import { docsUrl } from '../lib/origin';
import { useMode } from '../lib/mode';
import { TEST_EVENTS } from '../lib/copy';
import { keys, useApiKeys, useDeliveries, useSandboxInbox, useWebhookEndpoints } from '../lib/queries';
import { SandboxInboxSection, SandboxLiveNote, SendTestEventDialog } from './Sandbox';
import { DELIVERY_PILL, eventLabel, WEBHOOK_EVENT_TYPES } from '../lib/status';
import { ENABLE_NOTE, endpointPatch, endpointStatus, endpointUrlError, endpointUrlHint, isDisabled, PAUSE_NOTE, ROTATION_OVERLAP_NOTE, URL_EDIT_NOTE } from '../lib/webhooks';
import type { ApiKey, RollOverlap, WebhookEndpoint } from '../lib/types';

const WEBHOOK_EVENTS = WEBHOOK_EVENT_TYPES;

function SecretOnce({ secret, what, children }: { secret: string; what: string; children?: ReactNode }) {
  return (
    <div className="stack-md">
      <div className="callout" role="note">
        <Icon name="info" />
        <p>Copy this {what} now and keep it somewhere safe. You will not see it again.</p>
      </div>
      <CopyField value={secret} what={what} id="secret-value" />
      {children}
    </div>
  );
}

type AccessKind = 'full' | 'restricted';

function ScopePicker({ access, onChange }: { access: AccessMap; onChange: (a: AccessMap) => void }) {
  return (
    <div className="scope-picker" data-testid="scope-picker">
      {SCOPE_GROUPS.map((g) => (
        <Field key={g.resource} label={g.label}>
          {(p) => (
            <select {...p} className="select" value={access[g.resource] ?? 'none'} onChange={(e) => onChange({ ...access, [g.resource]: e.target.value as Access })}>
              <option value="none">No access</option>
              <option value="read">{g.readLabel}</option>
              {g.write ? <option value="write">{g.writeLabel}</option> : null}
            </select>
          )}
        </Field>
      ))}
    </div>
  );
}

function CreateKeyDialog({ onClose }: { onClose: () => void }) {
  const { mode } = useMode();
  const qc = useQueryClient();
  const step = useLiveConfirmation('api_key');
  const lock = useRecoveryLock();
  const [name, setName] = useState('');
  const [kind, setKind] = useState<AccessKind>('full');
  const [access, setAccess] = useState<AccessMap>(NO_ACCESS);
  const [submitted, setSubmitted] = useState(false);
  const scopes = kind === 'full' ? [...ALL_SCOPES] : scopesFromAccess(access);
  const create = useMutation({
    mutationFn: () => api.createApiKey({ name: name.trim(), scopes, mode, ...step.payload() }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: keys.apiKeys(mode) });
      step.done();
    },
    onError: (e) => (lock.absorbs(e) ? lock.onError(e) : step.onError(e)),
  });
  const nameError = submitted && !name.trim() ? 'Give the key a name, such as Production server.' : undefined;
  const scopeError = submitted && scopes.length === 0 ? 'Give the key access to at least one area.' : undefined;

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (lock.blocked()) return;
    setSubmitted(true);
    if (!name.trim() || scopes.length === 0) return step.showMissing();
    step.run(() => create.mutate());
  };

  if (create.data) {
    return (
      <Dialog title="Your new API key" onClose={onClose}>
        <SecretOnce secret={create.data.secret} what="API key">
          <p className="small muted">
            {create.data.name} · {mode === 'live' ? 'Live' : 'Test'} mode · {describeScopes(create.data.scopes)}
          </p>
          <div className="dialog-foot">
            <button type="button" className="btn btn-primary" onClick={onClose} data-autofocus>
              I copied the key
            </button>
          </div>
        </SecretOnce>
      </Dialog>
    );
  }

  return (
    <Dialog
      title="Create API key"
      onClose={onClose}
      busy={create.isPending}
      description={`The key works in ${mode === 'live' ? 'Live' : 'Test'} mode only. Use it from your server, never in a browser or app.`}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose} disabled={create.isPending}>
            Cancel
          </button>
          <button type="submit" form="key-form" className="btn btn-primary" disabled={create.isPending}>
            Create key
          </button>
        </>
      }
    >
      <form id="key-form" className="form-grid" onSubmit={submit} noValidate>
        <Field label="Name" error={nameError}>
          {(p) => <input {...p} className="input" autoComplete="off" value={name} onChange={(e) => setName(e.target.value)} placeholder="Production server" data-autofocus />}
        </Field>
        <fieldset className="plain">
          <legend>Access</legend>
          <div className="stack-sm">
            <Segmented
              legend="Access"
              className="segmented-block"
              value={kind}
              onChange={setKind}
              options={[
                { value: 'full', label: 'Full access' },
                { value: 'restricted', label: 'Restricted' },
              ]}
            />
            <p className="small muted" id="access-hint">
              {kind === 'full'
                ? 'Every API permission. Fine for your main server.'
                : 'Only what you choose below. Use a restricted key for a single job, such as a script that reads payments, so a leak exposes less.'}
            </p>
            {kind === 'restricted' ? <ScopePicker access={access} onChange={setAccess} /> : null}
          </div>
          <FieldError id="scope-error" message={scopeError} />
        </fieldset>
        {step.fields('key-step', 'Creating an API key')}
        {create.isError && !step.absorbs(create.error) && !lock.absorbs(create.error) ? <ErrorState error={create.error} /> : null}
      </form>
      {lock.dialog}
    </Dialog>
  );
}

function RollKeyDialog({ apiKey, onClose }: { apiKey: ApiKey; onClose: () => void }) {
  const { mode } = useMode();
  const qc = useQueryClient();
  const step = useLiveConfirmation('api_key', false);
  const lock = useRecoveryLock();
  const [overlap, setOverlap] = useState<RollOverlap>('24h');
  const label = apiKey.name ?? 'this key';
  const roll = useMutation({
    mutationFn: () => api.rollApiKey(mode, apiKey.id, { overlap, ...step.payload() }),
    onSuccess: () => void qc.invalidateQueries({ queryKey: keys.apiKeys(mode) }),
    onError: (e) => (lock.absorbs(e) ? lock.onError(e) : step.onError(e)),
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (lock.blocked()) return;
    if (step.check()) roll.mutate();
  };
  if (roll.data) {
    const prev = roll.data.previous;
    return (
      <Dialog title="New secret for your key" onClose={onClose}>
        <SecretOnce secret={roll.data.api_key.secret} what="API key">
          <p className="small muted" data-testid="roll-result">
            {prev.status === 'expiring'
              ? `The old secret keeps working until ${formatDateTime(prev.expires_at)}. Put the new one on your servers before then.`
              : 'The old secret no longer works.'}
          </p>
          <div className="dialog-foot">
            <button type="button" className="btn btn-primary" onClick={onClose} data-autofocus>
              I copied the key
            </button>
          </div>
        </SecretOnce>
      </Dialog>
    );
  }
  const chosen = ROLL_OVERLAPS.find((o) => o.value === overlap)!;
  return (
    <Dialog
      title={`Roll ${label}?`}
      onClose={onClose}
      busy={roll.isPending}
      description="Prismaport makes a new secret with the same name and permissions. The old secret keeps working for the time you choose, so you can switch without downtime."
      footer={
        <>
          <button type="button" className="btn" onClick={onClose} disabled={roll.isPending}>
            Cancel
          </button>
          <button type="submit" form="roll-form" className="btn btn-primary" disabled={roll.isPending}>
            Roll key
          </button>
        </>
      }
    >
      <form id="roll-form" className="form-grid" onSubmit={submit} noValidate>
        <fieldset className="plain">
          <legend>Old secret stops working</legend>
          <div className="stack-sm">
            <Segmented legend="Old secret stops working" className="segmented-block" value={overlap} onChange={setOverlap} options={ROLL_OVERLAPS.map((o) => ({ value: o.value, label: o.label }))} />
            <p className="small muted">{chosen.hint}</p>
          </div>
        </fieldset>
        {step.fields('roll-step', 'Rolling an API key')}
        {roll.isError && !step.absorbs(roll.error) && !lock.absorbs(roll.error) ? <ErrorState error={roll.error} /> : null}
      </form>
      {lock.dialog}
    </Dialog>
  );
}

function ApiKeys() {
  const { mode } = useMode();
  const q = useApiKeys();
  const qc = useQueryClient();
  const toast = useToast();
  const [creating, setCreating] = useState(false);
  const [rolling, setRolling] = useState<ApiKey | null>(null);
  const [revoking, setRevoking] = useState<ApiKey | null>(null);
  const revoke = useMutation({
    mutationFn: (k: ApiKey) => api.revokeApiKey(mode, k.id),
    onSuccess: (_d, k) => {
      void qc.invalidateQueries({ queryKey: keys.apiKeys(mode) });
      toast.show(`${k.name ?? 'API key'} revoked`);
      setRevoking(null);
    },
  });
  const rows = q.data?.data ?? [];
  return (
    <section className="stack-md" aria-labelledby="keys-title">
      <div className="row between">
        <div>
          <h2 className="heading" id="keys-title">
            API keys
          </h2>
          <p className="small muted">Secret keys for your server, in {mode === 'live' ? 'Live' : 'Test'} mode.</p>
        </div>
        <button type="button" className="btn btn-primary" onClick={() => setCreating(true)}>
          <Icon name="plus" />
          Create API key
        </button>
      </div>
      {q.isPending ? (
        <Loading what="API keys" />
      ) : q.isError ? (
        <ErrorState error={q.error} onRetry={() => q.refetch()} />
      ) : rows.length === 0 ? (
        <p className="card muted">No API keys in this mode yet.</p>
      ) : (
        <ul className="rows" aria-label="API keys">
          {rows.map((k) => {
            const pill = keyPill(k);
            const full = isFullAccess(k.scopes);
            return (
              <li key={k.id} className="simple-row" data-testid="api-key">
                <div className="stack-sm grow">
                  <p className="row">
                    <span className="subheading">{k.name ?? <span className="muted">Unnamed key</span>}</span>
                    <span className="tag">{full ? 'Full access' : 'Restricted'}</span>
                    {pill ? <Pill pill={pill} /> : null}
                  </p>
                  <p className="small muted row">
                    <span className="address">{k.prefix}…</span>
                    <span>Created {formatDateTime(k.created_at)}</span>
                    <span data-testid="last-used">{lastUsedLabel(k.last_used_at)}</span>
                    {k.status === 'revoked' && k.revoked_at ? <span>Revoked {formatDateTime(k.revoked_at)}</span> : null}
                  </p>
                  {full ? null : <p className="small">{describeScopes(k.scopes)}</p>}
                  <p className="row" aria-label="Scopes">
                    {k.scopes.map((s) => (
                      <span key={s} className="tag address">
                        {s}
                      </span>
                    ))}
                  </p>
                </div>
                {k.status === 'active' || k.status === 'expiring' ? (
                  <div className="actions">
                    {canRoll(k) ? (
                      <button type="button" className="btn btn-small" onClick={() => setRolling(k)} aria-label={`Roll ${k.name ?? 'this key'}`}>
                        <Icon name="rotate" />
                        Roll
                      </button>
                    ) : null}
                    <button type="button" className="btn btn-small" onClick={() => setRevoking(k)}>
                      Revoke
                    </button>
                  </div>
                ) : null}
              </li>
            );
          })}
        </ul>
      )}
      {creating ? <CreateKeyDialog onClose={() => setCreating(false)} /> : null}
      {rolling ? <RollKeyDialog apiKey={rolling} onClose={() => setRolling(null)} /> : null}
      {revoking ? (
        <ConfirmDialog
          title={`Revoke ${revoking.name ?? 'this key'}?`}
          confirmLabel="Revoke key"
          busy={revoke.isPending}
          onClose={() => setRevoking(null)}
          onConfirm={() => revoke.mutate(revoking)}
          body={
            <p>
              {revoking.status === 'expiring'
                ? 'This key was rolled and still works until its deadline. Revoking it stops it now. This cannot be undone.'
                : 'Requests with this key fail from now on. This cannot be undone; create a new key if you need one.'}
            </p>
          }
          error={revoke.isError ? <ErrorState error={revoke.error} /> : null}
        />
      ) : null}
    </section>
  );
}

function EventsPicker({ all, setAll, events, setEvents, error }: { all: boolean; setAll: (v: boolean) => void; events: string[]; setEvents: (f: (xs: string[]) => string[]) => void; error?: string }) {
  return (
          <fieldset className="plain">
            <legend>Events</legend>
            <div className="stack-sm">
              <label className="check">
                <input type="radio" name="events-mode" checked={all} onChange={() => setAll(true)} />
                <span>All events</span>
              </label>
              <label className="check">
                <input type="radio" name="events-mode" checked={!all} onChange={() => setAll(false)} />
                <span>Only these events</span>
              </label>
            </div>
            {!all ? (
              <div className="stack-sm mt-3">
                {WEBHOOK_EVENTS.map((ev) => (
                  <label key={ev} className="check">
                    <input type="checkbox" checked={events.includes(ev)} onChange={(e) => setEvents((xs) => (e.target.checked ? [...xs, ev] : xs.filter((x) => x !== ev)))} />
                    <span>
                      {eventLabel(ev)} <span className="address muted">{ev}</span>
                    </span>
                  </label>
                ))}
              </div>
            ) : null}
            <FieldError id="events-error" message={error} />
          </fieldset>
  );
}

function CreateEndpointDialog({ onClose }: { onClose: () => void }) {
  const { mode } = useMode();
  const qc = useQueryClient();
  const [url, setUrl] = useState('');
  const [description, setDescription] = useState('');
  const [all, setAll] = useState(true);
  const [events, setEvents] = useState<string[]>(['session.paid']);
  const [submitted, setSubmitted] = useState(false);
  // A Live endpoint receives every live payment event: it is confirmed with a code.
  const step = useLiveConfirmation('webhook_endpoint');
  const create = useMutation({
    mutationFn: () =>
      api.createWebhookEndpoint(mode, { url: url.trim(), enabled_events: all ? [] : events, ...(description.trim() ? { description: description.trim() } : {}), ...step.payload() }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: keys.webhooks(mode) });
      step.done();
    },
    onError: step.onError,
  });
  const urlProblem = endpointUrlError(url, mode);
  const urlError = submitted && urlProblem ? urlProblem : undefined;
  const eventsError = submitted && !all && events.length === 0 ? 'Choose at least one event, or send all events.' : undefined;
  const submit = (e: FormEvent) => {
    e.preventDefault();
    setSubmitted(true);
    if (urlProblem || (!all && events.length === 0)) return step.showMissing();
    step.run(() => create.mutate());
  };
  if (create.data) {
    return (
      <Dialog title="Webhook endpoint added" onClose={onClose}>
        <SecretOnce secret={create.data.secret} what="signing secret">
          <p className="small muted">Check the signature of every delivery with this secret before you trust it.</p>
          <div className="dialog-foot">
            <button type="button" className="btn btn-primary" onClick={onClose} data-autofocus>
              I copied the secret
            </button>
          </div>
        </SecretOnce>
      </Dialog>
    );
  }
  return (
    <Dialog
      title="Add webhook endpoint"
      onClose={onClose}
      busy={create.isPending}
      description={`Prismaport sends ${mode === 'live' ? 'Live' : 'Test'} mode events to this address and retries until it answers 2xx.`}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose} disabled={create.isPending}>
            Cancel
          </button>
          <button type="submit" form="endpoint-form" className="btn btn-primary" disabled={create.isPending}>
            Add endpoint
          </button>
        </>
      }
    >
      <form id="endpoint-form" className="form-grid" onSubmit={submit} noValidate>
        <Field label="Endpoint address" hint={endpointUrlHint(mode)} error={urlError}>
          {(p) => <input {...p} className="input mono" type="url" placeholder="https://acme.com/webhooks/prismaport" value={url} onChange={(e) => setUrl(e.target.value)} data-autofocus />}
        </Field>
        <Field label="Description" optional hint="A note for yourself, such as which service this feeds.">
          {(p) => <input {...p} className="input" type="text" maxLength={200} value={description} onChange={(e) => setDescription(e.target.value)} />}
        </Field>
        <EventsPicker all={all} setAll={setAll} events={events} setEvents={setEvents} error={eventsError} />
        {step.fields('endpoint-step', 'Adding a webhook endpoint')}
        {create.isError && !step.absorbs(create.error) ? <ErrorState error={create.error} /> : null}
      </form>
    </Dialog>
  );
}

function RotateDialog({ endpoint, onClose }: { endpoint: WebhookEndpoint; onClose: () => void }) {
  const { mode } = useMode();
  const qc = useQueryClient();
  const step = useLiveConfirmation('webhook_endpoint', false);
  const rotate = useMutation({
    mutationFn: () => api.rotateWebhookSecret(mode, endpoint.id, step.payload()),
    onSuccess: () => void qc.invalidateQueries({ queryKey: keys.webhooks(mode) }),
    onError: step.onError,
  });
  const confirm = () => {
    if (step.check()) rotate.mutate();
  };
  if (rotate.data) {
    const prev = rotate.data.previous_secret_expires_at;
    return (
      <Dialog title="New signing secret" onClose={onClose}>
        <SecretOnce secret={rotate.data.secret} what="signing secret">
          <p className="small muted">
            {prev ? `Deliveries carry signatures with both secrets until ${formatDateTime(prev)}, so you can switch without missing any.` : 'New deliveries are signed with this secret.'}
          </p>
          <div className="dialog-foot">
            <button type="button" className="btn btn-primary" onClick={onClose} data-autofocus>
              I copied the secret
            </button>
          </div>
        </SecretOnce>
      </Dialog>
    );
  }
  return (
    <ConfirmDialog
      title="Rotate the signing secret?"
      confirmLabel="Rotate secret"
      busy={rotate.isPending}
      onClose={onClose}
      onConfirm={confirm}
      body={
        <>
          <p>
            Prismaport makes a new secret for <span className="address">{endpoint.url}</span>. Update your server with it.
          </p>
          <p className="small muted">{ROTATION_OVERLAP_NOTE}</p>
          <form
            className="form-grid"
            noValidate
            onSubmit={(e) => {
              e.preventDefault();
              confirm();
            }}
          >
            {step.fields('rotate-step', 'Rotating a webhook secret')}
          </form>
        </>
      }
      error={rotate.isError && !step.absorbs(rotate.error) ? <ErrorState error={rotate.error} /> : null}
    />
  );
}

function EditEndpointDialog({ endpoint, onClose }: { endpoint: WebhookEndpoint; onClose: () => void }) {
  const { mode } = useMode();
  const qc = useQueryClient();
  const toast = useToast();
  const [url, setUrl] = useState(endpoint.url);
  const [description, setDescription] = useState(endpoint.description ?? '');
  const [all, setAll] = useState(endpoint.enabled_events.length === 0);
  const [events, setEvents] = useState<string[]>(endpoint.enabled_events.length === 0 ? ['session.paid'] : endpoint.enabled_events);
  const [submitted, setSubmitted] = useState(false);
  const patch = endpointPatch(endpoint, { url, events: all ? [] : events, description });
  const changed = Object.keys(patch).length > 0;
  // Moving a Live endpoint to another address takes the confirmation of creating one.
  const step = useLiveConfirmation('webhook_endpoint');
  const moving = patch.url !== undefined;
  const save = useMutation({
    mutationFn: () => api.updateWebhookEndpoint(mode, endpoint.id, { ...patch, ...(moving ? step.payload() : {}) }),
    onError: (e) => {
      if (moving) step.onError(e);
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: keys.webhooks(mode) });
      if (moving) step.done();
      toast.show('Webhook endpoint saved');
      onClose();
    },
  });
  const urlProblem = endpointUrlError(url, mode);
  const urlError = submitted && urlProblem ? urlProblem : undefined;
  const eventsError = submitted && !all && events.length === 0 ? 'Choose at least one event, or send all events.' : undefined;
  const submit = (e: FormEvent) => {
    e.preventDefault();
    setSubmitted(true);
    if (urlProblem || (!all && events.length === 0)) return moving ? step.showMissing() : undefined;
    if (!changed) return onClose();
    if (moving) return step.run(() => save.mutate());
    save.mutate();
  };
  return (
    <Dialog
      title="Edit webhook endpoint"
      onClose={onClose}
      busy={save.isPending}
      description="Changes apply to the next delivery attempt. Pending deliveries go to the new address."
      footer={
        <>
          <button type="button" className="btn" onClick={onClose} disabled={save.isPending}>
            Cancel
          </button>
          <button type="submit" form="edit-endpoint-form" className="btn btn-primary" disabled={save.isPending}>
            Save changes
          </button>
        </>
      }
    >
      <form id="edit-endpoint-form" className="form-grid" onSubmit={submit} noValidate>
        <Field label="Endpoint address" hint={endpointUrlHint(mode)} error={urlError}>
          {(p) => <input {...p} className="input mono" type="url" value={url} onChange={(e) => setUrl(e.target.value)} data-autofocus />}
        </Field>
        {patch.url ? (
          <div className="callout" role="note" data-testid="url-edit-note">
            <Icon name="info" />
            <p>{URL_EDIT_NOTE}</p>
          </div>
        ) : null}
        <Field label="Description" optional>
          {(p) => <input {...p} className="input" type="text" maxLength={200} value={description} onChange={(e) => setDescription(e.target.value)} />}
        </Field>
        <EventsPicker all={all} setAll={setAll} events={events} setEvents={setEvents} error={eventsError} />
        <p className="small muted">Event changes apply to events from now on. Deliveries already waiting are kept.</p>
        {moving ? step.fields('edit-endpoint-step', 'Changing a webhook address') : null}
        {save.isError && !(moving && step.absorbs(save.error)) ? <ErrorState error={save.error} /> : null}
      </form>
    </Dialog>
  );
}

/** Pause or enable an endpoint, with what each one does to events. */
function StatusDialog({ endpoint, next, onClose }: { endpoint: WebhookEndpoint; next: 'enabled' | 'disabled'; onClose: () => void }) {
  const { mode } = useMode();
  const qc = useQueryClient();
  const toast = useToast();
  const pausing = next === 'disabled';
  const set = useMutation({
    mutationFn: () => api.updateWebhookEndpoint(mode, endpoint.id, { status: next }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: keys.webhooks(mode) });
      toast.show(pausing ? 'Webhook endpoint paused' : 'Webhook endpoint enabled');
      onClose();
    },
  });
  return (
    <ConfirmDialog
      title={pausing ? 'Pause this endpoint?' : 'Enable this endpoint?'}
      confirmLabel={pausing ? 'Pause endpoint' : 'Enable endpoint'}
      busy={set.isPending}
      onClose={onClose}
      onConfirm={() => set.mutate()}
      body={
        <>
          <p>
            {pausing ? 'Prismaport stops sending events to ' : 'Prismaport starts sending events to '}
            <span className="address">{endpoint.url}</span>
            {pausing ? '.' : ' again.'}
          </p>
          <p className="small muted">{pausing ? PAUSE_NOTE : ENABLE_NOTE}</p>
        </>
      }
      error={set.isError ? <ErrorState error={set.error} /> : null}
    />
  );
}

function StatusPill({ endpoint }: { endpoint: WebhookEndpoint }) {
  const st = endpointStatus(endpoint);
  return (
    <>
      <Pill pill={{ label: st.label, tone: st.tone }} />
      {st.since ? (
        <span className="small muted" data-testid="endpoint-since">
          {st.state === 'paused' ? 'Since ' : st.state === 'kept_failing' ? 'Failing since ' : ''}
          {formatDateTime(st.since)}
        </span>
      ) : null}
    </>
  );
}

function Webhooks() {
  const { mode } = useMode();
  const q = useWebhookEndpoints();
  const qc = useQueryClient();
  const toast = useToast();
  const [creating, setCreating] = useState(false);
  const [rotating, setRotating] = useState<WebhookEndpoint | null>(null);
  const [deleting, setDeleting] = useState<WebhookEndpoint | null>(null);
  const [testing, setTesting] = useState<WebhookEndpoint | null>(null);
  const [editing, setEditing] = useState<WebhookEndpoint | null>(null);
  const [statusFor, setStatusFor] = useState<{ endpoint: WebhookEndpoint; next: 'enabled' | 'disabled' } | null>(null);
  const inboxEndpoint = useSandboxInbox().data?.inbox?.endpoint_id ?? null;
  const del = useMutation({
    mutationFn: (e: WebhookEndpoint) => api.deleteWebhookEndpoint(mode, e.id),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: keys.webhooks(mode) });
      void qc.invalidateQueries({ queryKey: keys.inbox(mode) });
      toast.show('Webhook endpoint deleted');
      setDeleting(null);
    },
  });
  const rows = q.data?.data ?? [];
  return (
    <section className="stack-md" aria-labelledby="hooks-title">
      <div className="row between">
        <div>
          <h2 className="heading" id="hooks-title">
            Webhook endpoints
          </h2>
          <p className="small muted">Your server hears about payments as they happen.</p>
        </div>
        <button type="button" className="btn btn-primary" onClick={() => setCreating(true)}>
          <Icon name="plus" />
          Add endpoint
        </button>
      </div>
      {q.isPending ? (
        <Loading what="webhook endpoints" />
      ) : q.isError ? (
        <ErrorState error={q.error} onRetry={() => q.refetch()} />
      ) : rows.length === 0 ? (
        <p className="card muted">No webhook endpoints in this mode yet.</p>
      ) : (
        <ul className="rows" aria-label="Webhook endpoints">
          {rows.map((e) => (
            <li key={e.id} className="simple-row" data-testid="webhook-endpoint">
              <div className="stack-sm grow">
                <p className="row">
                  <span className="address break">{e.url}</span>
                  {e.id === inboxEndpoint ? <span className="tag">{TEST_EVENTS}</span> : null}
                </p>
                {e.description ? <p className="small">{e.description}</p> : null}
                <p className="row" data-testid="endpoint-status">
                  <StatusPill endpoint={e} />
                </p>
                <p className="small muted">
                  {e.enabled_events.length === 0 ? 'All events' : e.enabled_events.map(eventLabel).join(', ')} · Added {formatDateTime(e.created_at)}
                </p>
                {e.previous_secret_expires_at ? (
                  <p className="small muted" data-testid="rotation-window">
                    The previous secret also signs deliveries until {formatDateTime(e.previous_secret_expires_at)}.
                  </p>
                ) : null}
              </div>
              <div className="actions">
                {mode === 'test' ? (
                  <button type="button" className="btn btn-small" onClick={() => setTesting(e)} aria-label={`Send test event to ${e.url}`}>
                    <Icon name="send" />
                    Send test event
                  </button>
                ) : null}
                <RouterLink to={`/developers/webhooks/${e.id}`} className="btn btn-small">
                  Deliveries
                </RouterLink>
                {e.id !== inboxEndpoint ? (
                  <>
                    <button type="button" className="btn btn-small" onClick={() => setEditing(e)} aria-label={`Edit ${e.url}`}>
                      <Icon name="edit" />
                      Edit
                    </button>
                    <button type="button" className="btn btn-small" onClick={() => setStatusFor({ endpoint: e, next: isDisabled(e) ? 'enabled' : 'disabled' })} aria-label={`${isDisabled(e) ? 'Enable' : 'Pause'} ${e.url}`}>
                      <Icon name="power" />
                      {isDisabled(e) ? 'Enable' : 'Pause'}
                    </button>
                  </>
                ) : null}
                <button type="button" className="btn btn-small" onClick={() => setRotating(e)}>
                  <Icon name="rotate" />
                  Rotate secret
                </button>
                <button type="button" className="btn btn-small" onClick={() => setDeleting(e)} aria-label={`Delete ${e.url}`}>
                  <Icon name="trash" />
                  Delete
                </button>
              </div>
            </li>
          ))}
        </ul>
      )}
      {creating ? <CreateEndpointDialog onClose={() => setCreating(false)} /> : null}
      {testing ? <SendTestEventDialog endpoint={testing} onClose={() => setTesting(null)} /> : null}
      {editing ? <EditEndpointDialog endpoint={editing} onClose={() => setEditing(null)} /> : null}
      {statusFor ? <StatusDialog endpoint={statusFor.endpoint} next={statusFor.next} onClose={() => setStatusFor(null)} /> : null}
      {rotating ? <RotateDialog endpoint={rotating} onClose={() => setRotating(null)} /> : null}
      {deleting ? (
        <ConfirmDialog
          title="Delete this endpoint?"
          confirmLabel="Delete endpoint"
          busy={del.isPending}
          onClose={() => setDeleting(null)}
          onConfirm={() => del.mutate(deleting)}
          body={
            <p>
              Prismaport stops sending events to <span className="address">{deleting.url}</span>. Payments are not affected.
            </p>
          }
          error={del.isError ? <ErrorState error={del.error} /> : null}
        />
      ) : null}
    </section>
  );
}

function WebhooksTab() {
  const { mode } = useMode();
  return (
    <div className="stack">
      <Webhooks />
      {mode === 'test' ? <SandboxInboxSection /> : <SandboxLiveNote />}
    </div>
  );
}

function DevTabs() {
  return (
    <nav className="tabs" aria-label="Developers">
      <NavLink to="/developers/api-keys" className="tab">
        API keys
      </NavLink>
      <NavLink to="/developers/webhooks" className="tab" end={false}>
        Webhooks
      </NavLink>
    </nav>
  );
}

export function DevelopersPage({ tab }: { tab: 'keys' | 'webhooks' }) {
  return (
    <>
      <PageHead
        title="Developers"
        lead="Connect your server to Prismaport with API keys and webhooks."
        actions={
          <a className="btn" href={docsUrl()} target="_blank" rel="noopener noreferrer">
            <Icon name="external" />
            API reference
          </a>
        }
      />
      <DevTabs />
      {tab === 'keys' ? <ApiKeys /> : <WebhooksTab />}
    </>
  );
}

export function WebhookDeliveriesPage() {
  const { id = '' } = useParams();
  const { mode, setMode } = useMode();
  // The warning email links here with ?mode=live|test: show that mode's endpoint.
  const [search] = useSearchParams();
  const wanted = search.get('mode');
  useEffect(() => {
    if ((wanted === 'live' || wanted === 'test') && wanted !== mode) setMode(wanted);
  }, [wanted, mode, setMode]);
  const endpoints = useWebhookEndpoints();
  const endpoint = endpoints.data?.data.find((e) => e.id === id);
  const q = useDeliveries(id);
  const qc = useQueryClient();
  const toast = useToast();
  const resend = useMutation({
    mutationFn: (deliveryId: string) => api.resendDelivery(mode, deliveryId),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: keys.deliveries(mode, id) });
      toast.show('Delivery queued again');
    },
  });
  const rows = q.data?.pages.flatMap((p) => p.data) ?? [];
  return (
    <>
      <RouterLink to="/developers/webhooks" className="back">
        <Icon name="back" />
        Webhooks
      </RouterLink>
      <PageHead title="Deliveries" lead={endpoint ? <span className="address">{endpoint.url}</span> : <ShortValue value={id} />} />
      {endpoint ? (
        <p className="row" data-testid="endpoint-status">
          <StatusPill endpoint={endpoint} />
        </p>
      ) : null}
      {endpoint && isDisabled(endpoint) ? (
        <div className="callout" role="note" data-testid="endpoint-disabled-note">
          <Icon name="info" />
          <p>
            This endpoint is {endpoint.disabled_reason === 'failing' ? 'disabled because it kept failing' : 'paused'}, so nothing is sent and deliveries cannot be resent. Enable it on the <RouterLink to="/developers/webhooks">Webhooks</RouterLink> page; deliveries that failed can be resent after that.
          </p>
        </div>
      ) : null}
      {resend.isError ? <ErrorState error={resend.error} /> : null}
      {q.isPending ? (
        <Loading what="deliveries" />
      ) : q.isError ? (
        <ErrorState error={q.error} onRetry={() => q.refetch()} />
      ) : rows.length === 0 ? (
        <p className="card muted">No deliveries to this endpoint yet.</p>
      ) : (
        <ul className="rows" aria-label="Deliveries">
          {rows.map((d) => (
            <li key={d.id} className="simple-row" data-testid="delivery">
              <div className="stack-sm grow">
                <p className="row">
                  <span className="subheading">{eventLabel(d.event_type)}</span>
                  <Pill pill={DELIVERY_PILL[d.status] ?? { label: d.status, tone: 'idle' }} />
                  {d.test ? <span className="tag">Test event</span> : null}
                </p>
                <p className="small muted row">
                  <ShortValue value={d.event_id} head={8} tail={4} />
                  <span>
                    {d.attempts} {d.attempts === 1 ? 'attempt' : 'attempts'}
                  </span>
                  {d.last_status_code !== null ? <span className="mono">HTTP {d.last_status_code}</span> : null}
                  <span>{d.last_attempt_at ? `Last tried ${formatDateTime(d.last_attempt_at)}` : `Created ${formatDateTime(d.created_at)}`}</span>
                  {d.status === 'pending' && d.next_attempt_at ? <span>Next try {formatDateTime(d.next_attempt_at)}</span> : null}
                </p>
                {d.last_error ? <p className="small address">{d.last_error}</p> : null}
              </div>
              {d.test ? (
                <span className="small muted">Sent once, not resent</span>
              ) : (
                <button type="button" className="btn btn-small" onClick={() => resend.mutate(d.id)} disabled={resend.isPending && resend.variables === d.id}>
                  <Icon name="send" />
                  Resend
                </button>
              )}
            </li>
          ))}
        </ul>
      )}
      {q.hasNextPage ? (
        <div className="more">
          <button type="button" className="btn" onClick={() => q.fetchNextPage()} disabled={q.isFetchingNextPage}>
            Show more deliveries
          </button>
        </div>
      ) : null}
    </>
  );
}

