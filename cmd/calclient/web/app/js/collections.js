// collections.js — collection listing helpers (calendar home, addressbook
// home, object listings) and the reusable calendar <select>.

import * as dav from '/shared/dav.js';
import { $, run, lastSegment } from './ui.js';
import { state } from './session.js';

const { NS, key, esc } = dav;

export async function homeChildren() {
  const r = await dav.propfind(state.calHome, [
    'd:resourcetype', 'd:displayname', 'a:calendar-color', 'c:calendar-description',
    'c:supported-calendar-component-set', 'cs:getctag', 'cs:source', 'd:sync-token',
  ], '1');
  return dav.msResponses(r.xml).filter(x => x.href !== state.calHome).map(x => {
    const rt = dav.resourceTypes(x.props.get(key(NS.d, 'resourcetype')));
    return {
      href: x.href,
      name: lastSegment(x.href),
      displayName: x.props.get(key(NS.d, 'displayname'))?.textContent ?? '',
      color: x.props.get(key(NS.a, 'calendar-color'))?.textContent ?? '',
      description: x.props.get(key(NS.c, 'calendar-description'))?.textContent ?? '',
      ctag: x.props.get(key(NS.cs, 'getctag'))?.textContent ?? '',
      syncToken: x.props.get(key(NS.d, 'sync-token'))?.textContent ?? '',
      source: dav.hrefText(x.props.get(key(NS.cs, 'source'))),
      components: [...(x.props.get(key(NS.c, 'supported-calendar-component-set'))?.children ?? [])]
        .map(c => c.getAttribute('name')).filter(Boolean),
      isCalendar: rt.includes(key(NS.c, 'calendar')),
      isSubscription: rt.includes(key(NS.cs, 'subscribed')),
    };
  });
}

export async function fillCalendarSelect(sel) {
  const cals = (await homeChildren()).filter(c => c.isCalendar);
  sel.innerHTML = cals.map(c =>
    `<option value="${esc(c.href)}">${esc(c.displayName || c.name)}</option>`).join('');
  return cals;
}

export async function bookChildren() {
  const r = await dav.propfind(state.abHome, ['d:resourcetype', 'd:displayname', 'cs:getctag'], '1');
  return dav.msResponses(r.xml).filter(x => x.href !== state.abHome).map(x => ({
    href: x.href,
    name: lastSegment(x.href),
    displayName: x.props.get(key(NS.d, 'displayname'))?.textContent ?? '',
    ctag: x.props.get(key(NS.cs, 'getctag'))?.textContent ?? '',
    isBook: dav.resourceTypes(x.props.get(key(NS.d, 'resourcetype'))).includes(key(NS.card, 'addressbook')),
  })).filter(x => x.isBook);
}

export async function listObjects(collection) {
  const r = await dav.propfind(collection, ['d:getetag', 'd:getcontenttype'], '1');
  return dav.msResponses(r.xml).filter(x => x.href !== collection && !x.href.endsWith('/')).map(x => ({
    href: x.href,
    name: lastSegment(x.href),
    etag: x.props.get(key(NS.d, 'getetag'))?.textContent ?? '',
    type: x.props.get(key(NS.d, 'getcontenttype'))?.textContent ?? '',
  }));
}

// calSelect is the markup for a calendar picker; wireCalSelect fills it and
// hooks up its refresh button, returning the <select>.
export const calSelect = `<label class="fld">calendar<select class="cal-sel"></select></label>
  <button class="cal-refresh" type="button">↻</button>`;

export function wireCalSelect(root, onReady) {
  const sel = $('.cal-sel', root);
  run($('.cal-refresh', root), () => fillCalendarSelect(sel));
  fillCalendarSelect(sel).then(onReady ?? (() => {}));
  return sel;
}
