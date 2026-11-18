import type { Mode, WebhookEndpointPatch } from './types';

/** How long a rotated-out secret keeps signing deliveries, next to the new one. */
export const ROTATION_OVERLAP_NOTE = 'For the next 24 hours every delivery is signed with both secrets, so your server keeps working while you switch.';

function loopback(host: string): boolean {
  const h = host.toLowerCase().replace(/\.$/, '');
  return h === 'localhost' || h.endsWith('.localhost') || h === '[::1]' || /^127(\.\d{1,3}){3}$/.test(h);
}

export const LIVE_URL_ERROR = 'Enter a full address that starts with https://.';
export const TEST_URL_ERROR = 'Enter a full address that starts with https://. In Test mode, http:// works for localhost too.';

/**
 * The server's endpoint URL rules, so the form says what is wrong before sending: https in
 * Live mode; in Test mode https, or plain http to this machine (localhost, a loopback IP).
 * The server has the final word (it also refuses private addresses in Live mode).
 */
export function endpointUrlError(raw: string, mode: Mode): string | undefined {
  const value = raw.trim();
  const refused = mode === 'live' ? LIVE_URL_ERROR : TEST_URL_ERROR;
  let u: URL;
  try {
    u = new URL(value);
  } catch {
    return refused;
  }
  if (/\s/.test(value) || u.username || u.password) return refused;
  if (u.protocol === 'https:' && u.hostname) return undefined;
  if (u.protocol === 'http:' && mode === 'test' && loopback(u.hostname)) return undefined;
  return refused;
}

export function endpointUrlHint(mode: Mode): string {
  return mode === 'live' ? 'An https:// address on your server.' : 'An https:// address, or http://localhost while you develop.';
}

type EndpointState = 'enabled' | 'failing' | 'paused' | 'kept_failing';

export interface EndpointStatus {
  state: EndpointState;
  label: string;
  tone: 'paid' | 'partial' | 'idle';
  /** The ISO time shown after the label, when the state has one. */
  since?: string;
}

/**
 * The status of an endpoint row: Enabled, Failing since (an open streak), Paused (the merchant
 * paused it) or Disabled: kept failing (Prismaport switched it off, with the streak's start).
 */
export function endpointStatus(e: { status?: string; disabled_reason?: string | null; failing_since?: string | null; disabled_at?: string | null }): EndpointStatus {
  if (e.status === 'disabled' || e.disabled_reason) {
    if (e.disabled_reason === 'failing') return { state: 'kept_failing', label: 'Disabled: kept failing', tone: 'idle', since: e.failing_since ?? undefined };
    return { state: 'paused', label: 'Paused', tone: 'idle', since: e.disabled_at ?? undefined };
  }
  if (e.failing_since) return { state: 'failing', label: 'Failing since', tone: 'partial', since: e.failing_since };
  return { state: 'enabled', label: 'Enabled', tone: 'paid' };
}

export function isDisabled(e: { status?: string; disabled_reason?: string | null }): boolean {
  return e.status === 'disabled' || Boolean(e.disabled_reason);
}

function sameSet(a: string[], b: string[]): boolean {
  return a.length === b.length && [...a].sort().join('\n') === [...b].sort().join('\n');
}

export interface EndpointForm {
  url: string;
  /** Empty means all events. */
  events: string[];
  description: string;
}

/** Only the fields the form changed, so an edit never resends what it did not touch. */
export function endpointPatch(orig: { url: string; enabled_events: string[]; description?: string | null }, form: EndpointForm): WebhookEndpointPatch {
  const patch: WebhookEndpointPatch = {};
  const url = form.url.trim();
  if (url !== orig.url) patch.url = url;
  if (!sameSet(form.events, orig.enabled_events)) patch.enabled_events = form.events;
  const desc = form.description.trim();
  if (desc !== (orig.description ?? '')) patch.description = desc === '' ? null : desc;
  return patch;
}

export const URL_EDIT_NOTE = 'Editing the address never changes the signing secret. If it moves to another party, rotate the secret too.';
export const PAUSE_NOTE = 'While paused, events are not sent to this endpoint, and they are not sent later either; read them from the events feed to catch up. Deliveries still waiting to be retried are dropped and marked failed.';
export const ENABLE_NOTE = 'Sending starts again with the next event. Nothing from the paused period is sent.';
