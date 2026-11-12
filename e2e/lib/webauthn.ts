// Chromium's VIRTUAL AUTHENTICATOR (the CDP WebAuthn domain), shared by the
// specs that exercise passkeys against the real GoTrue image.
//
// The authenticator is an internal (platform) CTAP2 one with resident keys and
// user verification, auto-confirming presence — a phone's or laptop's built-in
// passkey store, minus the human. It is attached to ONE page; each browser
// context gets its own, so two people's passkeys never meet unless a spec puts
// them on the same page on purpose.
//
// 🔴 AUTOFILL IS HELD BY DEFAULT. The sign-in page starts a conditional
// (autofill) navigator.credentials.get on load (passkeySignInJS), and with a
// virtual authenticator Chromium answers a conditional request AT ONCE with the
// first matching passkey — no suggestion list, no human to ignore it (measured
// on Chromium 153: resolved before the page could abort it). Left alone, every
// visit to /login on a page holding a passkey would sign in by itself. So
// authenticator() installs credentialsDouble, which keeps a conditional request
// PENDING until the page aborts it — what a real person who does not pick a
// suggestion leaves it as — and passes every modal request to the real
// authenticator unchanged. A spec that wants autofill to sign in asks for
// `autofill: 'real'` (recordWebAuthn).
import type { CDPSession, Page } from '@playwright/test';

export interface Authenticator {
  cdp: CDPSession;
  id: string;
}

// credentialsDouble wraps navigator.credentials.get in the page (an init
// script; it must be self-contained). Conditional requests are held unless the
// page's __mpkMode binding answers 'real'; each call, abort and settlement is
// reported to the __mpkReport binding when the spec installed one.
function credentialsDouble() {
  const w = window as any;
  const creds = navigator.credentials as any;
  if (!creds || w.__mpkDouble) return;
  w.__mpkDouble = true;
  const real = creds.get.bind(creds);
  const recs: any[] = [];
  const report = (e: any) => { if (typeof w.__mpkReport === 'function') w.__mpkReport(e); };
  creds.get = async function (opts?: any) {
    const pk = opts && opts.publicKey;
    const rec: any = {
      kind: 'get', n: recs.length, mediation: (opts && opts.mediation) || '',
      hints: pk && pk.hints ? Array.from(pk.hints) : null, hasSignal: !!(opts && opts.signal),
      // Was a conditional request still running when this one began?
      conditionalPending: recs.some((r) => r.mediation === 'conditional' && !r.settled && !r.aborted),
      aborted: false, settled: false,
    };
    recs.push(rec);
    report({ ...rec });
    const s = opts && opts.signal;
    if (s) s.addEventListener('abort', () => { rec.aborted = true; report({ kind: 'abort', n: rec.n }); });
    const mode = rec.mediation === 'conditional' && typeof w.__mpkMode === 'function' ? await w.__mpkMode() : 'hold';
    try {
      if (rec.mediation === 'conditional' && mode !== 'real') {
        return await new Promise((_, reject) => {
          if (!s) return;
          const stop = () => reject(new DOMException('The operation was aborted.', 'AbortError'));
          if (s.aborted) stop(); else s.addEventListener('abort', stop);
        });
      }
      const cred = await real(opts);
      report({ kind: 'settle', n: rec.n, outcome: 'ok', id: cred && cred.id });
      return cred;
    } catch (e: any) {
      report({ kind: 'settle', n: rec.n, outcome: e && e.name });
      throw e;
    } finally {
      rec.settled = true;
    }
  };
}

export async function authenticator(page: Page): Promise<Authenticator> {
  await page.addInitScript(credentialsDouble);
  const cdp = await page.context().newCDPSession(page);
  await cdp.send('WebAuthn.enable');
  const { authenticatorId } = await cdp.send('WebAuthn.addVirtualAuthenticator', {
    options: {
      protocol: 'ctap2', transport: 'internal', hasResidentKey: true,
      hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true,
    },
  });
  return { cdp, id: authenticatorId };
}

// signalDouble replaces (present) or removes (absent) the WebAuthn Signal API
// on PublicKeyCredential; replaced, each call is reported to __mpkReport.
function signalDouble(present: boolean) {
  const w = window as any;
  const P = w.PublicKeyCredential;
  if (!P) return;
  for (const name of ['signalAllAcceptedCredentials', 'signalUnknownCredential', 'signalCurrentUserDetails']) {
    if (present) {
      P[name] = (arg: any) => {
        if (typeof w.__mpkReport === 'function') w.__mpkReport({ kind: 'signal', name, arg: JSON.parse(JSON.stringify(arg)) });
        return Promise.resolve();
      };
    } else {
      try { delete P[name]; } catch (e) { /* not configurable: overwrite below */ }
      P[name] = undefined;
    }
  }
}

export interface WebAuthnRecord {
  // Every reported event, oldest first, across navigations.
  events: any[];
  // The autofill mode the double uses for the NEXT conditional request.
  autofill: 'hold' | 'real';
  // The sign-in page's status texts, as they changed.
  statuses: string[];
}

// recordWebAuthn reports page's WebAuthn calls to the spec (call it BEFORE
// authenticator(page) and before navigating): every navigator.credentials.get
// (mediation, hints, whether a conditional one was still pending), every abort
// and how each ended, and the Signal API — stubbed and recorded with
// signals 'stub', removed with 'absent' (the page must not depend on it).
export async function recordWebAuthn(page: Page, signals: 'stub' | 'absent'): Promise<WebAuthnRecord> {
  const rec: WebAuthnRecord = { events: [], autofill: 'hold', statuses: [] };
  await page.exposeBinding('__mpkReport', (_src, e) => { rec.events.push(e); });
  await page.exposeBinding('__mpkMode', () => rec.autofill);
  await page.exposeBinding('__mpkStatus', (_src, t: string) => { rec.statuses.push(t); });
  await page.addInitScript(signalDouble, signals === 'stub');
  await page.addInitScript(() => {
    document.addEventListener('DOMContentLoaded', () => {
      const el = document.getElementById('passkey-signin-status');
      const w = window as any;
      if (!el || typeof w.__mpkStatus !== 'function') return;
      new MutationObserver(() => w.__mpkStatus(el.textContent || '')).observe(el, { childList: true, characterData: true, subtree: true });
    });
  });
  return rec;
}

// toB64url is a CDP credential id (base64) in the Signal API's base64url.
export function toB64url(b64: string): string {
  return b64.replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

// credentialIds is the ids of the (resident) credentials the authenticator holds.
export async function credentialIds(a: Authenticator): Promise<string[]> {
  const { credentials } = await a.cdp.send('WebAuthn.getCredentials', { authenticatorId: a.id });
  return credentials.map((c) => c.credentialId);
}

export async function credentialCount(a: Authenticator): Promise<number> {
  return (await credentialIds(a)).length;
}

// setUserVerified makes the authenticator verify the user (PIN, biometric) or
// merely confirm presence on its next ceremonies.
export async function setUserVerified(a: Authenticator, verified: boolean): Promise<void> {
  await a.cdp.send('WebAuthn.setUserVerified', { authenticatorId: a.id, isUserVerified: verified });
}

export async function removeCredential(a: Authenticator, credentialId: string): Promise<void> {
  await a.cdp.send('WebAuthn.removeCredential', { authenticatorId: a.id, credentialId });
}
