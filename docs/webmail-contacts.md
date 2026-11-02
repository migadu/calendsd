# Webmail contacts

calendsd can be the contacts store behind a webmail, so the address book a
user sees in webmail is the same one their phone syncs over CardDAV. It was
built for [SnappyMail](https://snappymail.eu/), whose `AddressBookInterface`
the API mirrors one-to-one, but nothing in it is SnappyMail-specific beyond
that shape. There are two parts:

* a JSON **contacts API** for the webmail's address-book driver to call, and
* `calctl contacts import`, a one-off **migration** of the contacts the
  webmail already holds, read from an export bundle.

The webmail-side pieces (the PHP driver plugin and the exporter that writes
the bundle) are not part of this repository.

## The API

### Placement

The API is served by the calendsd binary on its own listener,
`server.api_addr` (e.g. `127.0.0.1:8081`, empty disables). A separate listener
rather than a path prefix on the DAV port means it is never reachable through
the public DAV hostname: bind it to loopback or a firewalled interface.

### Authentication

Basic auth with the **user's own mailbox credentials**, through the same
authentication backend instance the DAV server uses (rcptd or a passwd-file).

* No shared service secret on the webmail host that could impersonate every
  mailbox.
* The principal comes from the credential, so a handler cannot read another
  user's cards.
* It shares the backend's credential cache and auto-provisioning, and rejects
  soft-deleted accounts exactly as DAV does.

### Endpoints

| Method | Path | `AddressBookInterface` |
| --- | --- | --- |
| `GET` | `/api/v1/contacts?offset=&limit=&search=` | `GetContacts` (`total` answers the result-count parameter) |
| `GET` | `/api/v1/contacts/{id}` | `GetContactByID` |
| `GET` | `/api/v1/contacts/by-email?email=` | `GetContactByEmail` |
| `GET` | `/api/v1/contacts/by-uid/{uid}` | `GetContactByID` by string id |
| `POST` | `/api/v1/contacts` | `ContactSave` (create) → `{id, etag}` |
| `PUT` | `/api/v1/contacts/{id}` + `If-Match` | `ContactSave` (update) |
| `DELETE` | `/api/v1/contacts/{id}` | delete one |
| `POST` | `/api/v1/contacts/batch-delete` `{ids:[…]}` | `DeleteContacts` |
| `DELETE` | `/api/v1/contacts?all=1` | `DeleteAllContacts` (default book only) |
| `GET` | `/api/v1/suggestions?q=&limit=` | `GetSuggestions` → `[{email, name}]` |
| `POST` | `/api/v1/frecency` | `IncFrec` |
| `GET` | `/api/v1/health` | `Test` |

Addresses go in a query parameter rather than a path segment, since `+` and
`%` in local parts make path encoding error-prone.

A contact is `{id, uid, etag, changed, vcard}`. `If-Match` on `PUT` is
enforced atomically (`UPDATE … AND etag = $old`, `412` when it moved), so
webmail cannot clobber an edit a phone made in the meantime.

### Identity and multiple address books

`id` is the `cards` row's serial, a number, because SnappyMail runs contact
ids through `intval()` before its driver sees them. `uid` is the vCard UID.
The API also returns the card's `book` and `uri` for readable logs.

SnappyMail shows one flat list; a calendsd user can have several address
books:

* Reads aggregate across all of the principal's books.
* Creates go to the default book (`webmail`), falling back to the user's only
  book when they have exactly one under another name, and creating `webmail`
  when they have none.
* Updates and deletes target the book the card's `id` belongs to.
* `DeleteAllContacts` is scoped to the default book, so one click in webmail
  never wipes a book the user created elsewhere.

## Suggestions, frecency, and the indexing question

`IncFrec` ranks "most used" address suggestions and has no home in sabre's
schema, so it lives in calendsd's own `contact_frecency` table, keyed by
principal and lowercased address, with the display name parsed out of the
address webmail collected.

The count is split in two: `seed_frec` is owned by the importer and replaced
on every run, `frec` is owned by `IncFrec` and only ever incremented, and
ranking reads their sum. A single column would double every migrated count on
a re-run.

Search and suggestions scan the user's cards and frecency rows; a dedicated
index is deferred until measurements show it is needed.

## Migration

### The bundle

The exporter writes a directory, not a stream: produced once, checksummed,
then read as many times as the import needs.

```
manifest.json            run id, epoch, exporter version, per-shard digests
principals.tsv           principal, records, digest — one line each
cards/NNN.ndjson.gz      card records + a sentinel per principal
frecency/NNN.ndjson.gz   seeded usage counts
rejects.ndjson           contacts the exporter refused, with reasons
```

`NNN` is the first byte of `sha256(principal)` as three decimal digits, so
all of a principal's records live in one shard and each shard is importable
on its own. The record types are in `cmd/calctl/bundle.go`.

The importer treats each card as opaque bytes: it never parses or
re-serializes a vCard. The card bytes are therefore a pure function of the
source row and the exporter version, and `md5(carddata) == cards.etag` is an
exact "has this card changed" test.

### Making the import re-runnable

A re-run is either a no-op or a repair, never a duplication. One ledger row
per principal (`contact_import`) records the bundle digest imported, the
exporter version, and the book's synctoken right after the import. Two
comparisons then decide what run *N* does:

| Ledger row | Synctoken vs recorded | Bundle digest vs recorded | Action |
| --- | --- | --- | --- |
| absent | — | — | full import: principal, book, `COPY`, ledger |
| present | equal | equal | **skip**: two indexed reads, no writes |
| present | equal | differs | per-card diff (create / skip identical / update), no deletes |
| present | moved | equal | the user has been using the book; refresh the synctoken |
| present | moved | differs | **requires `--force-overwrite`** |
| present, exporter version differs | any | any | **requires `--force-version`** |

The per-card path is a diff, never a blind upsert: each write appends a
change row and a push notification, so rewriting identical cards would make
every device re-download everything.

Cards whose URI already exists in another of the principal's books (for
example, synced there by the webmail's own CardDAV client) are left out, so
they do not show up twice.

The import never deletes. A card present in calendsd but absent from the
bundle is reported, not removed: the importer cannot tell "deleted by the
user since" from "deleted in webmail after the export".
