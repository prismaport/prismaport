import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useState, type FormEvent } from 'react';
import { ConfirmDialog, Dialog } from '../components/Dialog';
import { Icon } from '../components/Icon';
import { useToast } from '../components/Toast';
import { CopyButton, CopyField, ErrorState, Field, Loading, Pill } from '../components/ui';
import { api } from '../lib/endpoints';
import { formatDateTime, formatRelative } from '../lib/format';
import { useMode } from '../lib/mode';
import { TEST_ADDRESS, TEST_EVENTS, TEST_EVENTS_CREATE, TEST_EVENTS_DELETE, TEST_EVENTS_DELETE_TITLE } from '../lib/copy';
import { keys, useInboxRequests, useSandboxInbox } from '../lib/queries';
import { formatSkew, prettyBody, SIGNATURE_FAILURE, signaturePill } from '../lib/sandbox';
import { DELIVERY_PILL, eventLabel, WEBHOOK_EVENT_TYPES } from '../lib/status';
import type { HeaderPair, InboxRequest, TestEventResult } from '../lib/types';


function formatBytes(n: number): string {
  return n < 1024 ? `${n} B` : `${(n / 1024).toFixed(1)} KB`;
}

/** A body or payload in monospace, indented when it is JSON, with a copy button. */
function CodeBlock({ text, label, empty = 'Empty body' }: { text: string; label: string; empty?: string }) {
  const { text: shown } = prettyBody(text);
  if (!shown) return <p className="small muted">{empty}</p>;
  return (
    <div className="code-wrap">
      <pre className="code-block" tabIndex={0} aria-label={label}>
        <code>{shown}</code>
      </pre>
      <CopyButton value={text} what={label.toLowerCase()} iconOnly className="btn-ghost btn-icon code-copy" />
    </div>
  );
}

function Headers({ headers, label }: { headers: HeaderPair[]; label: string }) {
  return (
    <dl className="headers" aria-label={label}>
      {headers.map((h, i) => (
        <div key={`${h.name}-${i}`} className="contents">
          <dt className="address">{h.name}</dt>
          <dd className="address">{h.value}</dd>
        </div>
      ))}
    </dl>
  );
}

function TestResult({ r }: { r: TestEventResult }) {
  const pill = DELIVERY_PILL[r.delivery.status] ?? { label: r.delivery.status, tone: 'idle' as const };
  return (
    <>
      <section className="stack-md" aria-labelledby="test-request-title">
        <div className="row">
          <Pill pill={pill} />
          <span className="small">
            {r.response ? `HTTP ${r.response.status} in ${r.response.latency_ms} ms` : 'No response'} · <span className="address">{r.event.type}</span>
          </span>
        </div>
        {r.error ? (
          <div className="callout" role="note">
            <Icon name="info" />
            <p>
              Your endpoint did not answer: <span className="address break">{r.error}</span>
            </p>
          </div>
        ) : null}
        <h3 className="subheading" id="test-request-title">
          Request
        </h3>
        <p className="small address break">
          {r.request.method} {r.request.url}
        </p>
        <Headers headers={r.request.headers} label="Request headers" />
        <CodeBlock text={r.request.body} label="Request body" />
      </section>
      <section className="stack-md" aria-labelledby="test-response-title">
        <h3 className="subheading" id="test-response-title">
          Response
        </h3>
        {r.response ? (
          <>
            <dl className="details">
              <dt>Status</dt>
              <dd className="mono">{r.response.status}</dd>
              <dt>Time</dt>
              <dd className="mono">{r.response.latency_ms} ms</dd>
            </dl>
            <CodeBlock text={r.response.body} label="Response body" />
            {r.response.body_truncated ? <p className="small muted">Showing the first 2 KB of the body.</p> : null}
          </>
        ) : (
          <p className="small muted">No HTTP response came back.</p>
        )}
        <p className="small muted">
          Listed in the deliveries of this endpoint as a test. It is not retried, and it is not in the event feed.
        </p>
      </section>
    </>
  );
}

/** Builds a sample event, signs it like a real delivery, sends it once and shows both sides. */
/** The test address is its only credential. */
export const INBOX_URL_HINT = "Anyone with this address can send to your test address. Don't share it.";

export function SendTestEventDialog({ endpoint, onClose }: { endpoint: { id: string; url: string }; onClose: () => void }) {
  const { mode } = useMode();
  const qc = useQueryClient();
  const [type, setType] = useState('session.paid');
  const send = useMutation({
    mutationFn: () => api.sendTestEvent(mode, endpoint.id, type),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: keys.deliveries(mode, endpoint.id) });
      void qc.invalidateQueries({ queryKey: keys.inboxRequests(mode) });
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    send.mutate();
  };
  return (
    <Dialog
      title="Send test event"
      variant="drawer"
      onClose={onClose}
      busy={send.isPending}
      description="Prismaport builds a sample event, signs it with this endpoint's secret like a real delivery and sends it once."
      footer={
        send.data ? (
          <>
            <button type="button" className="btn" onClick={() => send.reset()}>
              Send another
            </button>
            <button type="button" className="btn btn-primary" onClick={onClose}>
              Done
            </button>
          </>
        ) : (
          <>
            <button type="button" className="btn" onClick={onClose} disabled={send.isPending}>
              Cancel
            </button>
            <button type="submit" form="test-event-form" className="btn btn-primary" disabled={send.isPending}>
              {send.isPending ? <span className="spinner" aria-hidden="true" /> : <Icon name="send" />}
              Send test event
            </button>
          </>
        )
      }
    >
      {send.data ? (
        <TestResult r={send.data} />
      ) : (
        <form id="test-event-form" className="form-grid" onSubmit={submit} noValidate>
          <p className="small">
            To <span className="address break">{endpoint.url}</span>
          </p>
          <Field label="Event" hint="The sample is a Test mode session in the state this event announces, with fake but well-formed ids.">
            {(p) => (
              <select {...p} className="select" value={type} onChange={(e) => setType(e.target.value)} data-autofocus>
                {WEBHOOK_EVENT_TYPES.map((t) => (
                  <option key={t} value={t}>
                    {eventLabel(t)} ({t})
                  </option>
                ))}
              </select>
            )}
          </Field>
          {send.isError ? <ErrorState error={send.error} /> : null}
        </form>
      )}
    </Dialog>
  );
}

function InboxRequestItem({ r }: { r: InboxRequest }) {
  const sig = r.signature;
  const skew = formatSkew(sig.skew_seconds);
  return (
    <li className="inbox-row" data-testid="inbox-request">
      <details>
        <summary>
          <span className="stack-sm grow">
            <span className="row">
              <span className="subheading">{r.event_type ? eventLabel(r.event_type) : `${r.method} request`}</span>
              <Pill pill={signaturePill(sig)} />
            </span>
            <span className="small muted row">
              <span>{formatRelative(r.received_at)}</span>
              {r.event_type ? <span className="address break">{r.event_type}</span> : null}
              <span>{formatBytes(r.size)}</span>
            </span>
          </span>
          <Icon name="arrow" />
        </summary>
        <div className="inbox-detail stack-md">
          <div className="stack-sm" data-testid="signature-check">
            <h4 className="label">Signature</h4>
            <p className="small">{sig.valid ? 'Signed with this endpoint’s secret, within the 5-minute tolerance.' : SIGNATURE_FAILURE[sig.error ?? 'malformed']}</p>
            {sig.timestamp ? (
              <p className="small muted">
                Signed {formatDateTime(sig.timestamp)}
                {skew ? `, arrived ${skew}` : ''}
              </p>
            ) : null}
          </div>
          <div className="stack-sm">
            <h4 className="label">Headers</h4>
            <Headers headers={r.headers} label="Received headers" />
          </div>
          <div className="stack-sm">
            <h4 className="label">Body</h4>
            <CodeBlock text={r.body} label="Received body" />
          </div>
          <p className="small muted">
            Received {formatDateTime(r.received_at)}
            {r.event_id ? (
              <>
                {' '}
                · <span className="address break">{r.event_id}</span>
              </>
            ) : null}
          </p>
        </div>
      </details>
    </li>
  );
}

/** Test mode: a Prismaport-hosted endpoint that keeps what it receives, for merchants without a server yet. */
export function SandboxInboxSection() {
  const { mode } = useMode();
  const q = useSandboxInbox();
  const inbox = q.data?.inbox ?? null;
  const requests = useInboxRequests(Boolean(inbox));
  const qc = useQueryClient();
  const toast = useToast();
  const [secret, setSecret] = useState<string | null>(null);
  const [testing, setTesting] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const refresh = () => {
    void qc.invalidateQueries({ queryKey: keys.inbox(mode) });
    void qc.invalidateQueries({ queryKey: keys.webhooks(mode) });
  };
  const create = useMutation({
    mutationFn: () => api.createSandboxInbox(mode),
    onSuccess: (d) => {
      setSecret(d.inbox.secret ?? null);
      refresh();
    },
  });
  const del = useMutation({
    mutationFn: () => api.deleteSandboxInbox(mode),
    onSuccess: () => {
      setSecret(null);
      setDeleting(false);
      refresh();
      void qc.removeQueries({ queryKey: keys.inboxRequests(mode) });
      toast.show(`${TEST_EVENTS} address deleted`);
    },
  });
  const rows = requests.data?.pages.flatMap((p) => p.data) ?? [];
  return (
    <section className="stack-md" aria-labelledby="inbox-title" data-testid="sandbox-inbox">
      <div className="row between">
        <div>
          <h2 className="heading" id="inbox-title">
            {TEST_EVENTS}
          </h2>
          <p className="small muted">A Prismaport address that receives your Test mode webhooks and shows each request here, with its signature checked. No server needed.</p>
        </div>
        {inbox ? (
          <div className="actions">
            <button type="button" className="btn btn-small" onClick={() => setTesting(true)}>
              <Icon name="send" />
              Send test event
            </button>
            <button type="button" className="btn btn-small" onClick={() => setDeleting(true)}>
              <Icon name="trash" />
              {TEST_EVENTS_DELETE}
            </button>
          </div>
        ) : null}
      </div>
      {q.isPending ? (
        <Loading what="test webhook events" />
      ) : q.isError ? (
        <ErrorState error={q.error} onRetry={() => q.refetch()} />
      ) : !inbox ? (
        <div className="card stack-md">
          <p>Create a test address and Prismaport sends every Test mode event to it: test events, simulated payments and real testnet payments.</p>
          {create.isError ? <ErrorState error={create.error} /> : null}
          <div>
            <button type="button" className="btn btn-primary" onClick={() => create.mutate()} disabled={create.isPending}>
              {create.isPending ? <span className="spinner" aria-hidden="true" /> : <Icon name="plus" />}
              {TEST_EVENTS_CREATE}
            </button>
          </div>
        </div>
      ) : (
        <div className="card stack-md">
          <div className="stack-sm">
            <CopyField value={inbox.url} what={TEST_ADDRESS} id="inbox-url" />
            <p className="note" data-testid="inbox-url-hint">
              <Icon name="info" />
              <span>{INBOX_URL_HINT}</span>
            </p>
          </div>
          <p className="small muted">
            Keeps requests for {inbox.retention_days} days, the newest {inbox.max_requests}, up to {Math.round(inbox.max_body_bytes / 1024)} KB each. Refreshes every few
            seconds.
          </p>
          {secret ? (
            <div className="stack-sm">
              <p className="small">
                Signing secret of the test address, shown once. Prismaport checks every request with it for you; copy it if you also want to verify a request yourself.
              </p>
              <CopyField value={secret} what="signing secret" />
            </div>
          ) : null}
        </div>
      )}
      {inbox ? (
        requests.isPending ? (
          <Loading what="received requests" />
        ) : requests.isError ? (
          <ErrorState error={requests.error} onRetry={() => requests.refetch()} />
        ) : rows.length === 0 ? (
          <p className="card muted">Nothing received yet. Send a test event, or open a Test mode payment and simulate it.</p>
        ) : (
          <ul className="rows" aria-label="Received requests">
            {rows.map((r) => (
              <InboxRequestItem key={r.id} r={r} />
            ))}
          </ul>
        )
      ) : null}
      {requests.hasNextPage ? (
        <div className="more">
          <button type="button" className="btn" onClick={() => requests.fetchNextPage()} disabled={requests.isFetchingNextPage}>
            Show older requests
          </button>
        </div>
      ) : null}
      {testing && inbox ? <SendTestEventDialog endpoint={{ id: inbox.endpoint_id, url: inbox.url }} onClose={() => setTesting(false)} /> : null}
      {deleting ? (
        <ConfirmDialog
          title={TEST_EVENTS_DELETE_TITLE}
          confirmLabel={TEST_EVENTS_DELETE}
          busy={del.isPending}
          onClose={() => setDeleting(false)}
          onConfirm={() => del.mutate()}
          body={<p>Its address stops receiving events, its endpoint is deleted and the requests it kept are erased. You can create a new test address at any time.</p>}
          error={del.isError ? <ErrorState error={del.error} /> : null}
        />
      ) : null}
    </section>
  );
}

/** Live mode: where the sandbox tools are. */
export function SandboxLiveNote() {
  const { setMode } = useMode();
  return (
    <div className="callout" role="note" data-testid="sandbox-live-note">
      <Icon name="info" />
      <div className="stack-sm">
        <p>Test events, the test address and the payment simulator work in Test mode only, so nothing here can touch real payments.</p>
        <div>
          <button type="button" className="btn btn-small" onClick={() => setMode('test')}>
            Switch to Test mode
          </button>
        </div>
      </div>
    </div>
  );
}
