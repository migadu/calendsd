# Contact Deduplication — Design & Implementation Plan

## Problem

Address books accumulate several vCards for the same person — imports from
different sources, a card synced twice, an entry re-created because the original
wasn't found in search. There is no way today to detect or consolidate these.

Nothing in the current stack deduplicates contacts: the CardDAV server stores
each vCard as an opaque `carddata` blob keyed by `(addressbookid, uri)`, and the
client loads them into a flat `state.contacts` list. UID does **not** help —
duplicates are created by imports that mint fresh UIDs, so the same person has
several distinct UIDs.

## Approach: client-side, opt-in, review-based

Deduplication lives entirely in the web client. The architecture makes this the
natural fit:

- **Contacts are already fully in memory.** `loadContacts()` fetches every card
  and runs `parseContact()` into a rich model (`emails[]`, `tels[]`, `fn`,
  `first`/`last`, `org`, `uid`, …). That model is exactly what we compare.
- **Merge reuses existing primitives.** A merge is `saveContact()` (PUT the
  survivor) + `deleteContact()` (DELETE the losers) — both already exist in
  `shared/store/contacts.js`, with ETag conflict detection and a
  `loadContacts()` refresh for free.
- **The server has nothing to add.** vCards are opaque blobs with no indexed
  fields. Server-side dedup would mean re-parsing every blob and inventing a
  non-standard `/merge` endpoint. Keeping the server a plain CardDAV store is
  the right call.

Dedup is **opt-in** (a "Find duplicates" action) and **review-based** — no
silent auto-merge, because merging is destructive.

## Components

### 1. New module: `web/shared/store/dedup.js`

Pure functions over a contacts array — no DOM, **no imports from the rest of
`store/`** (`core.js` touches `localStorage` at module load, which would break
importing this module under node). That keeps it unit-testable in isolation
and matches the one-module-per-area split under `store/`.

**Normalization helpers**

| Helper | Behavior |
| --- | --- |
| `normEmail(v)` | lowercase, trim |
| `normPhone(v)` | strip to digits; compare on the last 9 digits so `+41…`/`0…` country-code variants still match; **ignore values under 7 digits** (extensions, junk like `0` or `-` would otherwise link unrelated cards); keep the original for display |
| `normName(fn, first, last)` | lowercase, strip accents (`normalize('NFD')` + drop combining marks), collapse whitespace and punctuation |

**Clustering strategy — strong links only are transitive**

Transitive closure over weak links is the classic dedup failure mode: a chain
A —email— B —name— C would pull the name-only C into a cluster the UI labels
"high confidence". So:

- **High-confidence clusters** come from union-find over the **strong signals
  only** (shared normalized email or phone). Only these merge transitively.
- **Low-confidence matches** (exact-normalized non-empty name, nothing
  stronger) are reported as separate **pairs** and never merge into — or
  bridge between — high-confidence clusters.

Matching is **within one address book**: partition `state.contacts` by
`c.book` before clustering (skip `kind === 'group'` cards). Cross-book dedup
is a later, explicitly opt-in mode.

**`findDuplicatesOf(contact, contacts) → Cluster | null`**

- The single-card check behind the per-contact affordance: returns the other
  cards that match `contact` under the same rules, with a `confidence`, or
  `null` if none. Cheap — the list is already in memory.

**`findDuplicateClusters(contacts) → Cluster[]`**

- The bulk sweep behind the Settings action; same rules, whole book.
- Returns only clusters/pairs with ≥2 members, highest-confidence / largest
  first.

**`mergePreview(cluster, survivorHref?) → mergedModel`**

- **Survivor is auto-picked, not chosen by the user.** Because every
  multi-valued field is unioned, the survivor only fixes the *kept identity*
  (uid/href/etag) and the structured address — both invisible — so the preview
  picks the most complete card (most emails/phones/scalars/address). The
  `survivorHref` argument is honored when passed (tests, or a future manual
  override) but is normally omitted.
- Union `emails` and `tels` (dedup by normalized value, keep first-seen
  `type`; the type is shown humanized — CELL as "Mobile" — in the preview).
- **Addresses:** the model carries a single structured `adr`
  (`parseContact`/`buildVCard` are not list-shaped). The merged card keeps the
  survivor's `adr` (or the first non-empty loser's); losers' further `ADR`
  lines are carried as `preserve` lines instead of being dropped.
- Scalars (`org`, `title`, `bday`, `url`, `note`): take the survivor's; if
  empty, fall back to the first non-empty loser. Record any **conflicting**
  non-empty scalars in `conflicts[]` so the UI can prompt. The **display name**
  is treated the same way — when the cards' `fn` differ it's a conflict, and
  each option in `nameOptions` carries its own `N` (first/last) so choosing one
  keeps them consistent.
- Keep the survivor's `uid` / `href` / `etag`; union in `preserve` lines
  unique to losers so unmodeled properties (PHOTO, X-*, …) aren't dropped.
  The union must **dedup by exact line and drop losers' single-valued
  properties the survivor already has** — naive concatenation yields invalid
  cards (two `PHOTO`s, and always two `REV`s, since `REV` is not in
  `MANAGED_VCARD` and so rides along in every card's `preserve`). Exclude
  `REV` entirely; the server/next client write regenerates it.

### 2. Merge execution — extend `store/contacts.js`

Add and export `mergeContacts(survivorModel, loserHrefs)`:

1. **Re-fetch first**: the preview was built from in-memory models that may be
   minutes old. `await loadContacts()` and re-derive the cluster (abort with a
   "changed meanwhile" message if it no longer matches) before writing.
2. `await saveContact(survivorModel)` — overwrites the survivor in place via
   its href + etag (the existing 412 conflict path).
3. **Rewrite group memberships**: for each `state.groups` card whose `members`
   reference a loser UID, replace it with the survivor UID and PUT the group
   card. Prevents dangling `X-ADDRESSBOOKSERVER-MEMBER` references.
   **This needs a dedicated group-save path** — `saveContact` runs through
   `buildVCard`, which is shaped for person cards (it emits `FN`/`N` from
   first/last and treats member lines as opaque `preserve` text). The rewrite
   is text surgery on the `X-ADDRESSBOOKSERVER-MEMBER` preserve lines (keeping
   the `urn:uuid:` prefix that `normalizeUID` strips) followed by an
   ETag-guarded raw PUT of the reconstructed card. Count it as its own edit
   with its own tests.
4. `deleteObject` each loser href **with `If-Match` on the loser's ETag** — a
   loser edited elsewhere since the re-fetch must 412, not be silently
   destroyed.
5. A single `loadContacts()` at the end (not once per operation).

The ordering makes a mid-merge failure safe: after any step the book contains
either the merged survivor plus not-yet-deleted losers (re-running dedup
re-detects and finishes the job) or the untouched originals. This idempotency —
not an undo — is the v1 safety story.

Re-export `mergeContacts`, `findDuplicateClusters`, `findDuplicatesOf`, and
`mergePreview` from the `shared/store.js` facade.

### 3. UI placement

Deduplication is a rare, bulk, destructive-adjacent **maintenance** action, not
a creational one. It deliberately does **not** go beside "New contact" in the
contacts toolbar — that bar is create + search, and a once-after-an-import
cleanup shouldn't claim prime real estate there or sit one button away from the
everyday add flow.

There are two entry points, mirroring how duplicates are actually noticed:

**Primary — per-contact, in the detail dialog (`contactDetail`).**
People notice a duplicate when looking at a specific person, not by reading a
global counter. So when a contact's detail card opens, run
`findDuplicatesOf(contact, store.state.contacts)`; if it returns matches, render
a subtle affordance near the header — e.g. "Possible duplicate of Jane Smith —
Review & merge" — that opens the merge dialog **scoped to that one cluster**.
This shows up only on the handful of cards that actually have a match, only when
opened — no global banner, no permanent toolbar button.

**Bulk — Settings, next to Import (`cal/js/settings.js`).**
Import is what *creates* duplicates and already lives in Settings alongside
Export; the whole-book cleanup is its natural counterpart. Add a **Contacts**
(or **Maintenance**) `<section>` right after Import with a "Find duplicate
contacts" action that runs `findDuplicateClusters(store.state.contacts)` and
opens the review dialog over every cluster at once — for the "I just imported a
file, clean it all up" case.

**Shared review dialog**, opened scoped-to-one-cluster from the detail card or
over all clusters from Settings. Build it with the current UI primitives: the
auto-escaping `html`/`frag` tags from `shared/dom.js`, `openOverlay` from
`shared/dialogs.js` for the scrim/Escape wiring (do not hand-wire the overlay
pattern), and `confirmDialog(..., { danger: true })` for the final merge
confirmation.

- A live preview of the single merged card, with a one-line note that emails,
  phones and addresses are combined. **No "keep as primary" picker** — the
  survivor is auto-chosen (see `mergePreview`), so the only inputs are the
  fields the cards actually disagree on.
- Each conflicting field (name, org, title, …) is a small "which value wins"
  `<select>`; everything else is read-only.
- Per-cluster **Merge**, with a `danger` confirm naming how many cards will be
  deleted.
- On confirm: `store.mergeContacts(...)`, then `toast('Contacts merged.')` and
  refresh from reloaded state. Errors surface via the existing `showError`.
- The Settings sweep title shows the count ("Duplicate contacts — N in M
  groups") and its list scrolls; empty state: "No duplicates found."

### 4. Tests — `cmd/calclient/tests/`

Unit-test the pure `dedup.js` logic with the existing harness (`cd
cmd/calclient && npm test` — node --test, zero setup, provided `dedup.js`
stays import-free as specified above):

- Phone / email / name normalization edge cases, including the sub-7-digit
  phone cutoff.
- Clustering: a shared email links two cards; name-only stays a `low` pair and
  does **not** bridge into a high cluster; cards in different books never
  match; genuinely distinct people don't merge.
- `mergePreview`: field union, conflict detection, `preserve` dedup (no
  doubled `REV`/`PHOTO`), single-`adr` handling.
- The group-membership rewrite: UID replaced in the member lines, `urn:uuid:`
  prefix intact, unrelated lines untouched.

## Matching summary

| Signal | Normalization | Strength |
| --- | --- | --- |
| Email | lowercase, trim | High — primary key, transitive |
| Phone | digits only, last-9 compare, ≥7 digits | High — transitive |
| Name (FN / N) | lowercase, de-accent, collapse | Low — pairs only, never transitive |

Deliberately **not** matched on UID alone — that is exactly what fails to catch
import-created duplicates.

## Default decisions (open to change)

- **Within-book only** to start (enforced by partitioning the input by
  `c.book`); cross-addressbook dedup is opt-in later. Avoids surprising
  cross-book merges.
- **Manual review always** — no silent auto-merge, even at high confidence.
- **No undo in v1** — the review step plus the idempotent merge ordering (see
  §2) is the safety net. If reversibility is ever wanted, prefer an
  export-before-merge (offer the losers as a downloadable `.vcf` before
  deleting) over stashing raw vCards in an `X-` property on the survivor —
  a loser with a base64 `PHOTO` would embed kilobytes of nested, re-escaped
  vCard into a single property line.

## Scope

One new file (`dedup.js`, ~120 lines of pure logic) + three focused edits
(`mergeContacts` and the group-save path in the store, the review dialog in the
UI) + unit tests. No database schema changes, no Go changes, no new protocol.
