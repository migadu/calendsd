// Unit tests for web/shared/dav.js's node-safe parts: the xml request-body
// tag (the XML twin of dom.js's frag — same escape-by-default contract),
// body builders, href helpers, and the drawer's pretty-printer.

import test from 'node:test';
import assert from 'node:assert/strict';
import { xml, propfindBody, prettyXML, lastSegment } from '../web/shared/dav.js';

test('xml escapes interpolated values by default', () => {
  assert.equal(String(xml`<d:displayname>${'a<b>&"c'}</d:displayname>`),
    '<d:displayname>a&lt;b&gt;&amp;&quot;c</d:displayname>');
});

test('xml composes fragments, arrays, and conditionals', () => {
  const hrefs = ['/a/', '/b<x>/'].map(h => xml`<d:href>${h}</d:href>`);
  assert.equal(String(xml`<d:prop>${hrefs}</d:prop>`),
    '<d:prop><d:href>/a/</d:href><d:href>/b&lt;x&gt;/</d:href></d:prop>');
  for (const on of [false, true]) {
    assert.equal(String(xml`<a>${on && xml`<b/>`}</a>`), on ? '<a><b/></a>' : '<a></a>');
  }
});

test('propfindBody lists the requested props', () => {
  const body = propfindBody(['d:resourcetype', 'cs:getctag']);
  assert.match(body, /<d:propfind /);
  assert.match(body, /<d:resourcetype\/><cs:getctag\/>/);
});

test('prettyXML indents nested elements', () => {
  const out = prettyXML('<a><b><c>text</c></b></a>');
  assert.equal(out, '<a>\n  <b>\n    <c>text\n    </c>\n  </b>\n</a>');
});

test('lastSegment decodes the final path segment', () => {
  assert.equal(lastSegment('/calendars/bob/team%20cal/'), 'team cal');
  assert.equal(lastSegment('/principals/alice/'), 'alice');
  assert.equal(lastSegment('/'), '');
  assert.equal(lastSegment(''), '');
});
