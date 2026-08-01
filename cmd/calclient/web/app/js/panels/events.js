// events.js — object listing plus an event editor exercising PUT with
// conditional headers (If-Match / If-None-Match) and iCal/jCal GETs.

import * as dav from '/shared/dav.js';
import * as ical from '/shared/ical.js';
import { $, el, run, setOut, statusMsg, pre, localDT, plusHours, icalToLocal } from '../ui.js';
import { primaryEmail } from '../session.js';
import { calSelect, wireCalSelect, listObjects } from '../collections.js';

const { esc } = dav;

export default {
  id: 'events', title: 'Events',
  render(main) {
    main.append(el(`<div>
      <h2>Events</h2>
      <fieldset><legend>Objects (PROPFIND depth 1)</legend>
        ${calSelect}
        <button class="primary" id="ev-list">List objects</button>
        <div class="out" id="ev-out"></div>
      </fieldset>
      <fieldset><legend>Editor (PUT with conditional headers)</legend>
        <label class="fld">uid<input id="ev-uid" size="38"></label>
        <button id="ev-newuid" type="button">new UID</button><br>
        <label class="fld">summary<input id="ev-sum" value="Demo event"></label>
        <label class="fld">start<input id="ev-start" type="datetime-local"></label>
        <label class="fld">end<input id="ev-end" type="datetime-local"></label>
        <label class="fld">RRULE<input id="ev-rrule" placeholder="FREQ=DAILY;COUNT=3" size="28"></label>
        <label class="fld">attendees (comma emails)<input id="ev-att" size="34"></label><br>
        <label class="fld">If-Match (etag)<input id="ev-ifmatch" size="30"></label>
        <label class="chk"><input type="checkbox" id="ev-ifnone">If-None-Match: * (create only)</label>
        <button class="primary" id="ev-put">PUT .ics</button>
        <div class="out" id="ev-put-out"></div>
      </fieldset>
      <div class="out" id="ev-view"></div>
    </div>`));

    $('#ev-uid', main).value = ical.newUID();
    $('#ev-start', main).value = localDT(plusHours(1));
    $('#ev-end', main).value = localDT(plusHours(2));
    run($('#ev-newuid', main), () => { $('#ev-uid', main).value = ical.newUID(); });

    const sel = wireCalSelect(main);
    const renderObjects = async () => {
      const objs = await listObjects(sel.value);
      const rows = objs.map(o => `<tr><td class="mono">${esc(o.name)}</td>
        <td class="mono">${esc(o.etag)}</td>
        <td><button data-get="${esc(o.href)}">GET iCal</button>
            <button data-jcal="${esc(o.href)}">GET jCal</button>
            <button data-load="${esc(o.href)}" data-etag="${esc(o.etag)}">Load</button>
            <button data-del="${esc(o.href)}">DELETE</button></td></tr>`).join('');
      setOut($('#ev-out', main), `<table class="list"><tr><th>object</th><th>etag</th><th></th></tr>${rows}</table>`);
      const out = $('#ev-out', main);
      for (const b of out.querySelectorAll('button[data-get]')) run(b, async () => {
        const r = await dav.req('GET', b.dataset.get);
        setOut($('#ev-view', main), statusMsg(r, r.headers.get('Content-Type')) + pre(r.text));
      });
      for (const b of out.querySelectorAll('button[data-jcal]')) run(b, async () => {
        const r = await dav.req('GET', b.dataset.jcal, { headers: { Accept: 'application/calendar+json' } });
        let body = r.text;
        try { body = JSON.stringify(JSON.parse(r.text), null, 2); } catch { /* leave as-is */ }
        setOut($('#ev-view', main), statusMsg(r, r.headers.get('Content-Type')) + pre(body));
      });
      for (const b of out.querySelectorAll('button[data-load]')) run(b, async () => {
        const r = await dav.req('GET', b.dataset.load);
        const ev = ical.components(ical.parse(r.text), 'VEVENT')[0];
        if (!ev) return;
        $('#ev-uid', main).value = ical.propValue(ev, 'UID');
        $('#ev-sum', main).value = ical.propValue(ev, 'SUMMARY').replace(/\\([\\;,n])/g, (_, c) => c === 'n' ? '\n' : c);
        $('#ev-start', main).value = icalToLocal(ical.propValue(ev, 'DTSTART')) || $('#ev-start', main).value;
        $('#ev-end', main).value = icalToLocal(ical.propValue(ev, 'DTEND')) || $('#ev-end', main).value;
        $('#ev-rrule', main).value = ical.propValue(ev, 'RRULE');
        $('#ev-ifmatch', main).value = r.headers.get('ETag') ?? b.dataset.etag;
      });
      for (const b of out.querySelectorAll('button[data-del]')) run(b, async () => {
        if (!confirm(`DELETE ${b.dataset.del}?`)) return;
        await dav.req('DELETE', b.dataset.del);
        await renderObjects();
      });
    };
    run($('#ev-list', main), renderObjects);

    run($('#ev-put', main), async () => {
      const uid = $('#ev-uid', main).value.trim();
      const body = ical.buildEvent({
        uid,
        summary: $('#ev-sum', main).value,
        start: $('#ev-start', main).value,
        end: $('#ev-end', main).value,
        rrule: $('#ev-rrule', main).value.trim(),
        organizer: $('#ev-att', main).value.trim() ? primaryEmail() : '',
        attendees: $('#ev-att', main).value.split(',').map(s => s.trim()).filter(Boolean),
      });
      const headers = { 'Content-Type': 'text/calendar; charset=utf-8' };
      if ($('#ev-ifmatch', main).value.trim()) headers['If-Match'] = $('#ev-ifmatch', main).value.trim();
      if ($('#ev-ifnone', main).checked) headers['If-None-Match'] = '*';
      const r = await dav.req('PUT', sel.value + encodeURIComponent(uid) + '.ics', { headers, body });
      const note = r.status === 412 ? 'precondition failed (stale If-Match / existing If-None-Match)' :
        `ETag ${r.headers.get('ETag') ?? '(none)'}`;
      setOut($('#ev-put-out', main), statusMsg(r, note));
      if (r.headers.get('ETag')) $('#ev-ifmatch', main).value = r.headers.get('ETag');
    });
  },
};
