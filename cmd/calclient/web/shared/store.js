// store.js — calweb's data layer: session, discovery, a per-calendar
// instance cache fed by server-side expanded calendar-queries, ctag polling,
// and ETag-checked writes. Views subscribe via `bus` and re-render.
//
// The implementation lives in store/, one module per feature area; this
// facade re-exports the public API so views import a single module. The
// explicit lists below are the API surface — a name not re-exported here is
// store-internal.

export { state, bus, savePrefs, DavError, friendly, myEmail } from './store/core.js';
export { haveSession, login, logout, restore, startPolling, stopPolling } from './store/session.js';
export { loadCalendars, eventCalendars, isReadOnly, calColor,
  createCalendar, updateCalendar, deleteCalendar } from './store/calendars.js';
export { setRange, refreshEvents, visibleInstances, moveEvent,
  eventHref, ridValue } from './store/events.js';
export { getObject, putObject, deleteObject, moveObject } from './store/objects.js';
export { shareCalendar, loadSharees, unshareCalendar,
  loadShareNotifications, replyShareInvite, dismissShareNotification } from './store/sharing.js';
export { loadInvites, answerInvite, answerEvent, dismissInvite } from './store/invites.js';
export { freeBusy } from './store/freebusy.js';
export { loadContacts, normalizeUID, saveContact, deleteContact,
  searchDirectory } from './store/contacts.js';
