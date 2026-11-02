# calclient UI backlog

Server features that work today but have no web-client UI — a user must know the
underlying magic address and an admin must provision the account by hand.

## Room / resource editor

Rooms are server-side today: a principal whose **username matches a configured
`auto_schedule_prefixes` entry** (e.g. `resource`, `location`) auto-accepts
invitations and declines on a free-busy conflict
(`schedule_delivery.go:autoProcess`). Setup is entirely out-of-band:

- add `auto_schedule_prefixes = ["resource", "location"]` to `config.toml`
- `calctl principal add resource-boardroom@domain --email ... --name "Board Room" --password ...`
- users book it by typing the room's address as an attendee

**Wanted in calclient:**
- create/edit/delete room accounts (admin-only) instead of `calctl`
- surface rooms in the attendee picker so users don't memorize addresses
- (nice-to-have) show the room's free/busy while composing so a decline is
  visible before sending

Note: rooms have no interactive login / reusable password — this is the same
account class that forces the relay fallback in `IMIP-SEND-AS-USER-PLAN.md`.

## Group-invite UI

A schedulable "group" is a **principal carrying a `{DAV:}group-member-set`**
(`schedule_principals.go:groupMembers`). With `expand_groups = true`, inviting
the group's address expands it into a display-only `X-SERVER-GROUP` placeholder
plus one real `ATTENDEE` per member (`expandGroupAttendees`, capped by
`maxExpandedAttendees`). This is a *principal* group, NOT a CardDAV contact
group — the two are unrelated today.

**Wanted in calclient:**
- create/edit group principals and their member sets (admin-only)
- surface groups in the attendee picker
- decide the relationship (if any) between CardDAV contact groups and schedulable
  principal groups — users will reasonably expect "invite this contact group" to
  work, and it currently doesn't

## Delegation / proxy access

The server supports calendar-user-proxy (`acl/proxy.go`) and the DAV dev console
has `panels/delegation.js`, but the end-user client (`web/cal/`) has **no
delegation UI at all** (`grep delegat|proxy` in `web/cal/js` → 0 hits).

**Wanted in calclient:**
- view and open calendars shared to me as a read/write delegate
- manage who may act on my behalf (grant/revoke proxy)

## Surface scheduling / iMIP delivery status

`web/cal/` never reads `schedule-status` (0 hits): when an invitation fails to
send, a room auto-declines, or — after `IMIP-SEND-AS-USER-PLAN.md` lands — the
user hits their outbound send limit, the client shows nothing. This is the
client-side counterpart to that plan's "client sees non-delivery" promise.

**Wanted in calclient:**
- show per-attendee delivery/response state on an event
- a clear error when a send is rejected (auth, over-limit, invalid recipient)

## To confirm (not yet scoped)

- **Reminders / alarms (VALARM)** — in flight now (`event-dialog.js` WIP, new
  `e2e/tests/reminder.spec.js`); track through to shipped rather than reopen.
- **Recurring-event edit scope** — partial RRULE support exists; verify the
  "this / this-and-future / all occurrences" edit flow is complete before
  assuming it's done.
