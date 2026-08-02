// scheduling.js — iTIP scheduling: outbox free-busy probe, sending an
// invite (PUT with ATTENDEEs), SCHEDULE-STATUS readback, and the inbox.

import * as dav from '/shared/dav.js';
import * as ical from '/shared/ical.js';
import { $, html, frag, run, setOut, statusMsg, pre, preXML, localDT, plusHours, rowsOr } from '../ui.js';
import { state, primaryEmail } from '../session.js';
import { calSelect, wireCalSelect, listObjects } from '../collections.js';

export default {
  id: 'scheduling', title: 'Scheduling',
  render(main) {
    main.append(html`<div>
      <h2>iTIP scheduling</h2>
      <fieldset><legend>Outbox free-busy probe (POST VFREEBUSY REQUEST)</legend>
        <label class="fld">attendees (comma emails)<input id="sc-fb-att" value="bob@example.org" size="34"></label>
        <label class="fld">start<input id="sc-fb-start" type="datetime-local"></label>
        <label class="fld">end<input id="sc-fb-end" type="datetime-local"></label>
        <button class="primary" id="sc-fb">POST outbox</button>
        <div class="out" id="sc-fb-out"></div>
      </fieldset>
      <fieldset><legend>Invite (PUT with ATTENDEEs → inbox delivery + SCHEDULE-STATUS)</legend>
        ${calSelect}
        <label class="fld">summary<input id="sc-sum" value="Planning meeting"></label>
        <label class="fld">start<input id="sc-start" type="datetime-local"></label>
        <label class="fld">end<input id="sc-end" type="datetime-local"></label>
        <label class="fld">attendees<input id="sc-att" value="bob@example.org" size="34"></label>
        <button class="primary" id="sc-invite">PUT invite</button>
        <button id="sc-status">Reload organizer copy (SCHEDULE-STATUS)</button>
        <div class="out" id="sc-inv-out"></div>
      </fieldset>
      <fieldset><legend>Inbox</legend>
        <button id="sc-inbox">PROPFIND inbox depth 1</button>
        <div class="out" id="sc-inbox-out"></div>
      </fieldset>
    </div>`);
    $('#sc-fb-start', main).value = localDT(plusHours(1));
    $('#sc-fb-end', main).value = localDT(plusHours(9));
    $('#sc-start', main).value = localDT(plusHours(24));
    $('#sc-end', main).value = localDT(plusHours(25));
    const sel = wireCalSelect(main);
    let lastInvite = null;

    run($('#sc-fb', main), async () => {
      const body = ical.buildFreeBusyRequest({
        organizer: primaryEmail(),
        attendees: $('#sc-fb-att', main).value.split(',').map(s => s.trim()).filter(Boolean),
        start: $('#sc-fb-start', main).value,
        end: $('#sc-fb-end', main).value,
      });
      const r = await dav.req('POST', state.outbox, {
        headers: { 'Content-Type': 'text/calendar; charset=utf-8' }, body,
      });
      setOut($('#sc-fb-out', main), frag`${statusMsg(r, 'schedule-response')}${r.xml ? preXML(r.text) : pre(r.text)}`);
    });

    run($('#sc-invite', main), async () => {
      const uid = ical.newUID();
      const body = ical.buildEvent({
        uid,
        summary: $('#sc-sum', main).value,
        start: $('#sc-start', main).value,
        end: $('#sc-end', main).value,
        organizer: primaryEmail(),
        attendees: $('#sc-att', main).value.split(',').map(s => s.trim()).filter(Boolean),
      });
      lastInvite = sel.value + uid + '.ics';
      const r = await dav.req('PUT', lastInvite, {
        headers: { 'Content-Type': 'text/calendar; charset=utf-8' }, body,
      });
      setOut($('#sc-inv-out', main), statusMsg(r, `stored ${lastInvite} — the server delivers copies to each attendee's inbox`));
    });

    run($('#sc-status', main), async () => {
      if (!lastInvite) { setOut($('#sc-inv-out', main), 'send an invite first'); return; }
      const r = await dav.req('GET', lastInvite);
      const ev = ical.components(ical.parse(r.text), 'VEVENT')[0];
      const rows = (ev?.props ?? []).filter(p => p.name === 'ATTENDEE').map(p =>
        frag`<tr><td class="mono">${p.value}</td><td class="mono">${p.params['SCHEDULE-STATUS'] ?? '(none)'}</td></tr>`);
      setOut($('#sc-inv-out', main), frag`${statusMsg(r, '')}
        <table class="list"><tr><th>attendee</th><th>SCHEDULE-STATUS</th></tr>${rows}</table>${pre(r.text)}`);
    });

    run($('#sc-inbox', main), async () => {
      const objs = await listObjects(state.inbox);
      const rows = objs.map(o => frag`<tr><td class="mono">${o.name}</td>
        <td><button data-get="${o.href}">GET</button>
            <button data-del="${o.href}">DELETE</button></td></tr>`);
      setOut($('#sc-inbox-out', main),
        frag`<table class="list"><tr><th>scheduling object</th><th></th></tr>${rowsOr(rows, 2)}</table><div class="out"></div>`);
      const box = $('#sc-inbox-out .out', main);
      for (const b of $('#sc-inbox-out', main).querySelectorAll('button[data-get]')) run(b, async () => {
        const r = await dav.req('GET', b.dataset.get);
        setOut(box, pre(r.text));
      });
      for (const b of $('#sc-inbox-out', main).querySelectorAll('button[data-del]')) run(b, async () => {
        await dav.req('DELETE', b.dataset.del);
        $('#sc-inbox', main).click();
      });
    });
  },
};
