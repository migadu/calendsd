// Unit tests for the escaping core of web/shared/dom.js. html`...` needs a
// browser document, but esc/frag/raw are pure string logic — and they are the
// safety property the whole UI now leans on, so they get pinned here.

import test from 'node:test';
import assert from 'node:assert/strict';
import { esc, frag, raw, brJoin } from '../web/shared/dom.js';

test('esc escapes the four HTML metacharacters', () => {
  assert.equal(esc(`<a href="x">&'`), '&lt;a href=&quot;x&quot;&gt;&amp;\'');
});

test('frag escapes interpolated values by default', () => {
  assert.equal(String(frag`<b>${'<script>alert(1)</script>'}</b>`),
    '<b>&lt;script&gt;alert(1)&lt;/script&gt;</b>');
  assert.equal(String(frag`<i title="${'"><img onerror=x>'}">t</i>`),
    '<i title="&quot;&gt;&lt;img onerror=x&gt;">t</i>');
});

test('frag passes through nested frag / raw unescaped', () => {
  assert.equal(String(frag`<div>${frag`<b>${'<x>'}</b>`}</div>`),
    '<div><b>&lt;x&gt;</b></div>');
  assert.equal(String(frag`<div>${raw('<hr>')}</div>`), '<div><hr></div>');
});

test('frag flattens arrays and drops null/undefined/booleans', () => {
  assert.equal(String(frag`<ul>${[frag`<li>a</li>`, frag`<li>b</li>`]}</ul>`),
    '<ul><li>a</li><li>b</li></ul>');
  assert.equal(String(frag`x${null}${undefined}${false}${true}y`), 'xy');
  assert.equal(String(frag`n=${0}`), 'n=0');   // falsy numbers still render
});

test('conditional markup composes with && without leaking "false"', () => {
  const on = true, off = false;
  assert.equal(String(frag`<p>${on && frag`<b>yes</b>`}</p>`), '<p><b>yes</b></p>');
  assert.equal(String(frag`<p>${off && frag`<b>yes</b>`}</p>`), '<p></p>');
});

test('brJoin escapes each line and joins with <br>', () => {
  assert.equal(String(brJoin(['a<b', 'c'])), 'a&lt;b<br>c');
});
