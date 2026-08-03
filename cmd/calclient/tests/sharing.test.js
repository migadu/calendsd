// Unit tests for web/shared/store/sharing.js. These pin the DAV requests the
// module builds — the {cs}share / {cs}remove / {cs}invite-reply bodies and the
// escape-by-default contract — plus the "nothing pending" error branches, by
// stubbing global fetch and inspecting the exchange. The XML-parsing paths
// (loadSharees roster, invite-notification cards) need a DOM and are covered by
// the sharing e2e spec (owner roster) instead.

import test from 'node:test';
import assert from 'node:assert/strict';
import * as sharing from '../web/shared/store/sharing.js';
import { state } from '../web/shared/store/core.js';

// A fetch stub that records each exchange and answers by method: POST→200,
// DELETE→204, everything else (PROPFIND/GET)→an empty 207 multistatus so the
// follow-up loads inside replyShareInvite complete without throwing.
function stubFetch() {
  const calls = [];
  globalThis.fetch = async (url, opts = {}) => {
    const method = opts.method || 'GET';
    calls.push({ method, url, body: opts.body ?? null });
    const status = method === 'POST' ? 200 : method === 'DELETE' ? 204 : 207;
    return new Response('', { status });
  };
  return calls;
}

let savedFetch;
test.beforeEach(() => {
  savedFetch = globalThis.fetch;
  globalThis.sessionStorage = {
    getItem: () => null, setItem: () => {}, removeItem: () => {},
  };
  state.calHome = '/calendars/bob/';
  state.shareNotifications = [];
});
test.afterEach(() => {
  globalThis.fetch = savedFetch;
  delete globalThis.sessionStorage;
});

test('shareCalendar POSTs a {cs}share/set with a mailto href and read-write grant', async () => {
  const calls = stubFetch();
  await sharing.shareCalendar('/calendars/bob/work/', { email: 'user01@example.org', readWrite: true });

  assert.equal(calls.length, 1);
  const { method, url, body } = calls[0];
  assert.equal(method, 'POST');
  assert.equal(url, '/calendars/bob/work/');
  assert.match(body, /<cs:share\b/);
  assert.match(body, /<cs:set>/);
  assert.match(body, /<d:href>mailto:user01@example\.org<\/d:href>/);
  assert.match(body, /<cs:read-write\/>/);         // "can edit"
});

test('shareCalendar omits read-write for a read-only invite', async () => {
  const calls = stubFetch();
  await sharing.shareCalendar('/calendars/bob/work/', { email: 'r@x.org' });   // readWrite defaults false
  assert.doesNotMatch(calls[0].body, /<cs:read-write\/>/);
});

test('shareCalendar carries the summary, escaped', async () => {
  const calls = stubFetch();
  await sharing.shareCalendar('/calendars/bob/work/',
    { email: 'user01@example.org', summary: 'Team <plans> & more' });
  assert.match(calls[0].body, /<cs:summary>Team &lt;plans&gt; &amp; more<\/cs:summary>/);
});

test('shareCalendar escapes the invitee address by default', async () => {
  const calls = stubFetch();
  await sharing.shareCalendar('/calendars/bob/work/', { email: 'a<b&c@x.org' });
  assert.match(calls[0].body, /mailto:a&lt;b&amp;c@x\.org/);
  assert.doesNotMatch(calls[0].body, /a<b&c/);      // raw metacharacters never reach the wire
});

test('unshareCalendar POSTs a {cs}share/remove for the sharee', async () => {
  const calls = stubFetch();
  await sharing.unshareCalendar('/calendars/bob/work/', 'user01@example.org');
  assert.equal(calls[0].method, 'POST');
  assert.match(calls[0].body, /<cs:remove><d:href>mailto:user01@example\.org<\/d:href><\/cs:remove>/);
});

test('replyShareInvite POSTs an {cs}invite-reply carrying hosturl, in-reply-to and the answer', async () => {
  const calls = stubFetch();
  const notif = { href: '/calendars/bob/notifications/n1.xml', uid: 'uid-42',
    hostHref: '/calendars/alice/team/' };

  await sharing.replyShareInvite(notif, true);

  const reply = calls[0];
  assert.equal(reply.method, 'POST');
  assert.equal(reply.url, '/calendars/bob/');       // posted to the sharee's own home
  assert.match(reply.body, /<cs:invite-reply\b/);
  assert.match(reply.body, /<cs:hosturl><d:href>\/calendars\/alice\/team\/<\/d:href><\/cs:hosturl>/);
  assert.match(reply.body, /<cs:in-reply-to>uid-42<\/cs:in-reply-to>/);
  assert.match(reply.body, /<cs:invite-accepted\/>/);
  // The accept then dismisses the notification (DELETE) and refreshes.
  assert.ok(calls.some(c => c.method === 'DELETE' && c.url === notif.href));
});

test('replyShareInvite sends invite-declined when the invitation is refused', async () => {
  const calls = stubFetch();
  await sharing.replyShareInvite({ href: '/n/2.xml', uid: 'u2', hostHref: '/calendars/alice/team/' }, false);
  assert.match(calls[0].body, /<cs:invite-declined\/>/);
  assert.doesNotMatch(calls[0].body, /<cs:invite-accepted\/>/);
});

test('loadSharees returns an empty roster when the calendar reports no invite prop', async () => {
  globalThis.fetch = async () => new Response('', { status: 404 });   // non-207
  assert.deepEqual(await sharing.loadSharees('/calendars/bob/gone/'), []);
});

test('loadShareNotifications clears the inbox and emits when the collection 500s', async () => {
  // The server 500s /notifications/ when it has no inbox; treat as "nothing pending".
  globalThis.fetch = async () => new Response('boom', { status: 500 });
  state.shareNotifications = [{ uid: 'stale' }];                       // must be cleared

  await sharing.loadShareNotifications();
  assert.deepEqual(state.shareNotifications, []);
});
