// objects.js — raw object-level DAV operations (GET/PUT/DELETE/MOVE on a
// single .ics/.vcf resource), with ETag-conditional writes.

import * as dav from '../dav.js';
import { check } from './core.js';

export async function getObject(href) {
  const r = check(await dav.req('GET', href), [200]);
  return { text: r.text, etag: r.headers.get('ETag') ?? '' };
}

export async function putObject(href, text, etag, contentType = 'text/calendar; charset=utf-8') {
  const headers = { 'Content-Type': contentType };
  if (etag) headers['If-Match'] = etag;
  else if (etag === null) headers['If-None-Match'] = '*';
  return check(await dav.req('PUT', href, { headers, body: text }), [201, 204]);
}

// deleteObject removes a resource. Pass an etag to make the DELETE conditional
// (If-Match) so a resource edited elsewhere since we last read it 412s instead
// of being silently destroyed — the merge path relies on this.
export async function deleteObject(href, etag) {
  const headers = etag ? { 'If-Match': etag } : {};
  check(await dav.req('DELETE', href, { headers }), [204]);
}

// moveObject relocates an object to another collection via WebDAV MOVE — the
// path for changing an event's calendar. The server re-homes the object
// without re-sending scheduling messages (MOVE is exempt from the CANCEL that
// a DELETE would fire). Destination must be an absolute URI. Overwrite is left
// at its default (true): the destination filename is UID-derived, so a name
// clash means a stale copy of the same event (e.g. from an interrupted move) —
// replacing it is correct, and refusing (Overwrite:F) would 412 forever.
export async function moveObject(href, destHref) {
  const dest = new URL(destHref, location.origin).href;
  check(await dav.req('MOVE', href, { headers: { Destination: dest } }), [201, 204]);
}
