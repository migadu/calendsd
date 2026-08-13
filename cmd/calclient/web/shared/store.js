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
export { loadCalendars, eventCalendars, isReadOnly, calColor, calInk, inkOn,
  createCalendar, updateCalendar, deleteCalendar, reorderCalendars } from './store/calendars.js';
export { setRange, refreshEvents, visibleInstances, searchEvents, resolveEvent, setCalendarHidden, moveEvent,
  eventHref, ridValue, instanceKey, deleteEvent, resendInvitations } from './store/events.js';
export { getObject, putObject, deleteObject, moveObject } from './store/objects.js';
export { loadDelegates, grantDelegate, revokeDelegate, resolvePrincipal } from './store/proxy.js';
export { shareCalendar, loadSharees, unshareCalendar,
  loadShareNotifications, replyShareInvite, dismissShareNotification } from './store/sharing.js';
export { loadInvites, answerInvite, answerEvent, dismissInvite, removeCancelled } from './store/invites.js';
export { freeBusy } from './store/freebusy.js';
export { loadContacts, initContacts, ensureFullContact, normalizeUID, saveContact, deleteContact, mergeContacts,
  createBook, renameBook, deleteBook, createGroup, renameGroup, deleteGroup, searchDirectory } from './store/contacts.js';
export { findDuplicatesOf, findDuplicateClusters, mergePreview } from './store/dedup.js';
export { startImport, cancelImport, importRunning } from './store/import.js';
export { taskLists, writableTaskLists, loadTasks, dueBucket,
  createTask, updateTask, setTaskDone, deleteTask, createTaskList } from './store/tasks.js';
