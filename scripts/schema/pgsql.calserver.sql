-- calserver extension schema: tables sabre/dav's PDO backend does not define.
--
-- Sabre's CalDAV\Backend\PDO implements SharingSupport but deliberately leaves
-- NotificationSupport (the notification inbox + shareReply) and calendar
-- publishing unimplemented, so its schema has no home for them. calserver
-- fills those gaps here.
--
-- Every object below is a NEW table: a live sabre instance running against the
-- same database never references them, so loading this alongside the sabre
-- schema cannot affect it. No sabre-owned table is altered except to widen a
-- varchar (see the end of the file). Load this AFTER the
-- dav/examples/sql/pgsql.*.sql files.
--
-- THIS FILE IS SAFE TO RE-RUN. Every statement is conditional, so applying it
-- to a database that already has some or all of it is a no-op that takes no
-- exclusive lock. Run it with ON_ERROR_STOP so a real failure stops the run:
--
--     psql -v ON_ERROR_STOP=1 -d sabredav -f scripts/schema/pgsql.calserver.sql
--
-- What re-runnable does NOT mean: this is not a migration runner. CREATE TABLE
-- IF NOT EXISTS matches on the name, so a table that already exists with an
-- OUTDATED definition is skipped silently, not brought up to date. The guard at
-- the end of the file turns that specific failure into a loud one for the
-- tables here; changing an existing column is still a deliberate ALTER you
-- write yourself.

-- PRECONDITION. Guard against the one hazard CREATE TABLE IF NOT EXISTS
-- introduces, and do it before anything is applied.
--
-- IF NOT EXISTS matches on the table NAME only. A table left over from an
-- earlier, different version of this file is silently skipped, and the run
-- reports success while the schema is wrong — which is worse than the duplicate-
-- table error it replaced, because that error was at least loud. So any table
-- this file owns that ALREADY exists is checked for the columns the current
-- code requires, and a mismatch stops the run here rather than half way through.
--
-- A table that does not exist yet is not checked: it is about to be created.
--
-- The list covers the tables most likely to drift, not every column in the
-- file. Extend it when you add a column — that is the step that turns "the file
-- ran" back into "the schema is what the code expects".
DO $$
DECLARE
    missing text;
BEGIN
    SELECT string_agg(v.tbl || '.' || v.col, ', ' ORDER BY v.tbl, v.col)
      INTO missing
      FROM (VALUES
        ('contact_frecency', 'principal'), ('contact_frecency', 'email'),
        ('contact_frecency', 'name'),      ('contact_frecency', 'frec'),
        ('contact_frecency', 'seed_frec'), ('contact_frecency', 'last_used'),
        ('contact_import',   'principal'), ('contact_import',   'source_users'),
        ('contact_import',   'book_id'),   ('contact_import',   'book_uri'),
        ('contact_import',   'bundle_digest'), ('contact_import', 'exporter_ver'),
        ('contact_import',   'cards_total'),   ('contact_import', 'cards_written'),
        ('contact_import',   'cards_skipped'), ('contact_import', 'synctoken'),
        ('contact_import',   'run_id'),        ('contact_import', 'imported_at')
      ) AS v(tbl, col)
     WHERE to_regclass(v.tbl) IS NOT NULL
       AND NOT EXISTS (
        SELECT 1 FROM pg_attribute a
         WHERE a.attrelid = to_regclass(v.tbl)
           AND a.attname = v.col
           AND a.attnum > 0 AND NOT a.attisdropped);

    IF missing IS NOT NULL THEN
        RAISE EXCEPTION
            'schema is out of date, these columns are missing: %. Those tables already exist, so CREATE TABLE IF NOT EXISTS would skip them and this file would report success over a wrong schema. Drop or ALTER them deliberately, then re-run.',
            missing;
    END IF;
END $$;


-- Per-principal notification inbox (calendarserver invite notifications). One
-- row per delivered invite; entryid is the stable id that also names the
-- <principal>/notifications/<entryid>.xml resource.
CREATE TABLE IF NOT EXISTS notifications (
    id SERIAL NOT NULL,
    principaluri VARCHAR(100) NOT NULL,
    entryid VARCHAR(60) NOT NULL,
    dtstamp BIGINT NOT NULL,            -- unix seconds (BIGINT: Y2038-safe)
    hosturl TEXT NOT NULL,              -- shared calendar path (base-relative)
    organizer VARCHAR(100) NOT NULL,    -- sharer principal path
    organizername VARCHAR(100),
    access SMALLINT NOT NULL,           -- 2 = read, 3 = readwrite
    invitestatus SMALLINT NOT NULL,     -- 1 = noresponse, 2 = accepted, 3 = declined, 4 = invalid
    summary TEXT,
    -- Declared inline rather than as a separate ALTER TABLE ADD CONSTRAINT:
    -- Postgres has no ADD CONSTRAINT IF NOT EXISTS, and this file is meant to
    -- be safe to re-run. An inline table primary key is named <table>_pkey, the
    -- same name the ALTER assigned, so the catalog is unchanged.
    PRIMARY KEY (id)
);

CREATE UNIQUE INDEX IF NOT EXISTS notifications_principaluri_entryid
    ON notifications USING btree (principaluri, entryid);


-- Published calendars. A row exists exactly while a calendar is published;
-- publicuri is the opaque public path segment (a UUID, as sabre would mint).
-- The FK to calendars cascades on delete, so removing a calendar drops its
-- publication without the core DeleteCalendar path having to know this table
-- exists (keeping sabre-only databases valid).
CREATE TABLE IF NOT EXISTS calendarpublications (
    calendarid INTEGER NOT NULL REFERENCES calendars (id) ON DELETE CASCADE,
    publicuri VARCHAR(100) NOT NULL,
    PRIMARY KEY (calendarid)   -- inline, see notifications above
);


-- Pending account deletions (soft-delete tombstones). One row exists while an
-- account is scheduled for deletion but not yet purged. The auth plugin
-- rejects logins for a tombstoned principal (immediate disable), while its
-- data stays untouched until the retention window elapses and the purge job
-- (calctl principal purge-expired) runs DeleteUserCascade. purge_after is
-- materialized at schedule time so changing the retention config never moves
-- an already-scheduled purge. No FK to principals: the tombstone must survive
-- a manually-removed principal (the purge job tolerates that case).
CREATE TABLE IF NOT EXISTS deleted_principals (
    principal    VARCHAR(200) PRIMARY KEY,   -- "principals/alice"
    username     VARCHAR(50)  NOT NULL,      -- bare username, for the purge cascade
    deleted_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    purge_after  TIMESTAMPTZ  NOT NULL,      -- deleted_at + retention window
    reason       TEXT
);

CREATE INDEX IF NOT EXISTS deleted_principals_purge_after_ix
    ON deleted_principals USING btree (purge_after);


-- WebDAV-Push subscriptions (PUSH-PLAN.md). One row per (collection, Web Push
-- endpoint): a client POSTs a {https://bitfire.at/webdav-push}push-register body
-- to a calendar/addressbook collection and the dispatcher (internal/push) sends
-- an encrypted Web Push message when that collection's synctoken changes. This
-- is a NEW calserver-only table; a co-resident sabre never references it.
--
-- collection_id is the numeric calendars.id / addressbooks.id (as text) the
-- pg_notify('dav_push', 'calendar:<id>') payload carries. token is the opaque,
-- random id that names the subscription resource for DELETE (unsubscribe).
-- last_synctoken is seeded to the collection's current synctoken at register
-- time so the startup catch-up sweep only fires for changes actually missed.
CREATE TABLE IF NOT EXISTS push_subscriptions (
    id               BIGSERIAL PRIMARY KEY,
    token            TEXT NOT NULL,             -- opaque resource id (Location)
    collection_kind  TEXT NOT NULL,             -- 'calendar' | 'addressbook'
    collection_id    TEXT NOT NULL,             -- numeric calendars/addressbooks id
    collection_path  TEXT NOT NULL,             -- base-relative path (for ACL re-check)
    principal_uri    TEXT NOT NULL,             -- subscriber (for ACL re-check + scoping)
    push_resource    TEXT NOT NULL,             -- Web Push endpoint (RFC 8030)
    p256dh           TEXT NOT NULL,             -- subscription public key
    auth_secret      TEXT NOT NULL,             -- subscription auth secret
    last_synctoken   BIGINT NOT NULL DEFAULT 0, -- last synctoken pushed to this sub
    expires          TIMESTAMPTZ NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Re-registering the same endpoint on the same collection updates the row
    -- (keys/expiry) rather than duplicating it (WebDAV-Push §5 idempotency).
    UNIQUE (collection_kind, collection_id, push_resource)
);

CREATE UNIQUE INDEX IF NOT EXISTS push_subscriptions_token ON push_subscriptions (token);
CREATE INDEX IF NOT EXISTS push_subscriptions_lookup
    ON push_subscriptions (collection_kind, collection_id);


-- Durable background-job queue (JOB-QUEUE-PLAN.md). A NEW calserver-only table;
-- a co-resident sabre never references it. Oban-shaped so a future Elixir/Oban
-- migration is a column mapping. iMIP send is the first (and, for now, only)
-- worker: it survives a process restart the in-memory SendQueue could not.
--
-- INVARIANTS:
--  (1) args is inspectable JSONB and MUST NOT contain a secret (D8). The
--      submission credential lives ONLY in enc_credential, AEAD-encrypted (D12).
--  (2) enc_credential is set only for a failing per-user (submit-as-user) send:
--      NULLed the moment the send succeeds ('completed'); retained until the TTL
--      purge sweep on 'discarded' so a manual `calctl imip resend` can reuse it.
--      It is always NULL for relay-identity jobs (server mail, resource replies).
CREATE TABLE IF NOT EXISTS jobs (
    id             BIGSERIAL   PRIMARY KEY,
    queue          TEXT        NOT NULL DEFAULT 'default',
    worker         TEXT        NOT NULL,                     -- logical name, e.g. "imip_send"
    args           JSONB       NOT NULL,                     -- worker payload; NO secret (D8)
    state          TEXT        NOT NULL DEFAULT 'available', -- available|executing|completed|discarded
    attempt        INT         NOT NULL DEFAULT 0,
    max_attempts   INT         NOT NULL DEFAULT 6,
    scheduled_at   TIMESTAMPTZ NOT NULL DEFAULT now(),       -- earliest next run (backoff)
    attempted_at   TIMESTAMPTZ,                              -- current 'executing' claim start (D9 reclaim)
    last_error     TEXT,                                     -- most recent failure, for ops
    enc_credential BYTEA,                                    -- AEAD ciphertext (D12); NULL unless a per-user send is pending/given-up
    enc_nonce      BYTEA,                                    -- per-row AEAD nonce
    key_id         SMALLINT,                                 -- which config key encrypted it (rotation)
    inserted_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at   TIMESTAMPTZ                               -- when it reached a terminal state (completed|discarded)
);

-- Claim: due, available jobs for a queue, oldest first (D9 claim loop).
CREATE INDEX IF NOT EXISTS jobs_claim_ix   ON jobs (queue, state, scheduled_at);
-- Reclaim: orphaned 'executing' rows a dead worker never finished (D9 sweep).
CREATE INDEX IF NOT EXISTS jobs_reclaim_ix ON jobs (state, attempted_at);

-- Coalesce: at most one pending job per (uid, recipient, method). A newer
-- revision supersedes the pending one via ON CONFLICT DO UPDATE, ordered by
-- (SEQUENCE, DTSTAMP) from args (D10); an older/equal re-issue is a no-op. A
-- REQUEST's SEQUENCE is client-controlled (echoed, may be non-monotonic); a
-- server-generated CANCEL's is incremented (monotonic) — so SEQUENCE is an
-- ordering key with DTSTAMP as tiebreaker, never the identity. Scoped to
-- 'available' so a supersede racing an in-flight send inserts fresh. Rows
-- without these args keys (a future non-iMIP worker) have all-NULL keys, which
-- a unique index treats as distinct, so they never collide.
CREATE UNIQUE INDEX IF NOT EXISTS jobs_coalesce_ix ON jobs (
    (args->>'uid'), (args->>'recipient'), (args->>'method')
) WHERE state = 'available';


-- Drop sabre's over-strict calendarinstances_principaluri_share_href unique
-- index. A sharee row stores the invitee's address (e.g. mailto:bob@…) in
-- share_href and their principal in principaluri, both identical across every
-- calendar shared with that person — so this index makes the SECOND share to a
-- given sharee fail with a duplicate-key error, i.e. you can share at most one
-- calendar with any one person. The value is faithful to sabre's own PDO
-- backend, so sabre's pgsql example schema carries the same latent bug; its
-- own tests never share two calendars with one principal, so it went unseen.
-- Real integrity is still enforced by calendarinstances_principaluri_calendarid
-- (a calendar can't be bound into a home twice) and _principaluri_uri (unique
-- instance path); nothing in calserver or sabre depends on this index, so
-- dropping it is safe for a co-resident sabre and fixes the same bug for it.
DROP INDEX IF EXISTS calendarinstances_principaluri_share_href;


-- Widen the columns that hold a principal's email address. sabre's stock schema
-- sizes users.username at VARCHAR(50) and principals.uri/email/displayname at
-- 200/80/80, which predates using the address itself as the username. With
-- rcptd authentication the provisioner writes the full email into users.username
-- and into both principals columns, and builds principals.uri as
-- 'principals/' || email (plus a '/calendar-proxy-write' suffix on the sub-
-- principals, so 11 + 254 + 21 = 286 characters at the RFC 5321 limit).
--
-- Any mailbox with a longer address therefore failed its INSERT, and because
-- provisioning is fail-closed (a provision error is reported as an auth
-- failure, see auth.RcptdBackend.Check) those accounts could not use CalDAV or
-- CardDAV at all. 770 of them on the production host. Nothing had to be cleaned
-- up afterwards: CreateUser runs inside a transaction, so each failed attempt
-- rolled back whole and the account provisions normally on its next login.
--
-- Widening a varchar is metadata-only in Postgres (no table rewrite, no index
-- rebuild), and the values are faithful to sabre's own PDO backend, so a
-- co-resident sabre sharing this database is unaffected and gets the same fix.
-- (applied by the conditional block at the end of this file)


-- The same widening, one level out: the columns that hold a principaluri or a
-- username on collections and tombstones rather than on the principal itself.
--
-- addressbooks.principaluri is sabre's stock VARCHAR(255), but a principaluri
-- is 'principals/' || email = 11 + 254 = 265 characters at the RFC 5321 limit.
-- Nothing hit it while address books were only ever created lazily for users
-- who did CardDAV discovery, because those users could not provision at all
-- until the block above was applied. A bulk import that creates a book per
-- principal hits it directly.
--
-- deleted_principals was added by calserver (SOFT-DELETE-PLAN.md) with column
-- widths copied from the pre-widening sabre schema, so it would refuse the
-- soft-delete of exactly the long-address accounts the block above unblocked.
--
-- Metadata-only, same as above.
-- (applied by the conditional block at the end of this file)


-- Per-correspondent usage counts behind webmail's "most used" address
-- suggestions (docs/snappymail-calserver-contacts.md). SnappyMail's own driver
-- keeps this as a prop_frec column on its normalized property rows, which has
-- no home in sabre's schema — sabre stores whole vCards and nothing per
-- address. A NEW calserver-only table; a co-resident sabre never references it.
--
-- email is the lowercased address and the ranking key. SnappyMail's IncFrec
-- matches prop_value case-sensitively, so `Bob@x` and `bob@x` accumulate two
-- independent counts there; folding them is deliberate, and it is why the
-- migration's seed pre-aggregates per lowercased address before writing.
--
-- The count is split in two columns on purpose, and it is what makes the
-- migration re-runnable:
--   seed_frec  owned by `calctl contacts import`, REPLACED on every run
--   frec       owned by the API's IncFrec, only ever incremented
-- A single summed column would double on a second import run; ranking reads
-- (frec + seed_frec), so the split costs nothing and the seed stays idempotent.
--
-- name is the display name parsed out of the address SnappyMail collected
-- (\MailSo\Mime\Email::Parse), or the contact's FN for a migrated row. NULL
-- when the source carried none — a suggestion is then address-only, which is
-- honest; inventing a name from the local part is not.
CREATE TABLE IF NOT EXISTS contact_frecency (
    principal  VARCHAR(320) NOT NULL,      -- 'principals/alice@example.org'
    email      VARCHAR(320) NOT NULL,      -- lowercased address; the ranking key
    name       VARCHAR(255),               -- parsed display name, NULL if unknown
    frec       INTEGER      NOT NULL DEFAULT 0,  -- live IncFrec count (API-owned)
    seed_frec  INTEGER      NOT NULL DEFAULT 0,  -- migrated prop_frec (importer-owned)
    last_used  TIMESTAMPTZ,                -- last IncFrec; NULL for a seeded row
    PRIMARY KEY (principal, email)
);

-- Suggestions are "this principal's addresses, most used first", which is the
-- one query this table exists to serve.
CREATE INDEX IF NOT EXISTS contact_frecency_rank_ix
    ON contact_frecency (principal, ((frec + seed_frec)) DESC);


-- Import ledger for the webmail contacts migration: one row per principal
-- whose contacts `calctl contacts import` has landed
-- (docs/snappymail-calserver-contacts.md § Making the import re-runnable). A
-- NEW calserver-only table; a co-resident sabre never references it.
--
-- The row is what makes a re-run a no-op instead of a duplication. Two stored
-- values decide it: bundle_digest (did the source change?) and synctoken (did
-- a DAV client change the destination since?). Equal/equal is a skip costing
-- two indexed reads and no writes, which is the difference between re-running
-- the import casually and scheduling it.
--
-- It is also the proof of ownership. A row exists only for a book this import
-- created, so a `webmail` address book with no ledger row is the user's own and
-- is refused rather than merged into — hence no "was this ours" column.
--
-- exporter_ver pins the serializer. The skip decision compares md5(carddata)
-- against cards.etag, which is only meaningful while the same contact
-- serializes to the same bytes; a SnappyMail or Sabre\VObject upgrade between
-- runs would make every digest differ for reasons that are not data, so a
-- bundle from a different version is refused unless --force-version.
--
-- No foreign key to principals or addressbooks: the ledger is evidence, and it
-- must survive the principal being purged. It is deliberately NOT in
-- AdminWrites.DeleteUserCascade for the same reason — revisit that only as a
-- conscious decision about how long the evidence should live.
CREATE TABLE IF NOT EXISTS contact_import (
    principal      VARCHAR(320) PRIMARY KEY,  -- 'principals/alice@example.org'
    source_users   INTEGER[]    NOT NULL,     -- the rainloop_users id(s) folded in
    book_id        INTEGER      NOT NULL,     -- addressbooks.id the cards landed in
    book_uri       VARCHAR(200) NOT NULL,     -- 'webmail'
    bundle_digest  CHAR(64)     NOT NULL,     -- the per-principal sentinel digest imported
    exporter_ver   TEXT         NOT NULL,     -- exporter + Sabre\VObject version behind it
    cards_total    INTEGER      NOT NULL,
    cards_written  INTEGER      NOT NULL,     -- cumulative across runs
    cards_skipped  INTEGER      NOT NULL,     -- last run: identical etag, no write
    synctoken      INTEGER      NOT NULL,     -- the book's synctoken right after import
    run_id         UUID         NOT NULL,
    imported_at    TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS contact_import_run_ix ON contact_import (run_id);


-- Apply the column widenings described above, but only where they are still
-- needed.
--
-- A bare ALTER TABLE ... ALTER COLUMN TYPE is idempotent in effect, but not in
-- cost: it takes a brief ACCESS EXCLUSIVE lock on the table whether or not the
-- column already has the width asked for. This file is meant to be re-runnable
-- against a live database, so widening only what is narrow keeps a no-op re-run
-- genuinely free — it reads pg_attribute and touches nothing.
--
-- atttypmod - 4 is the declared varchar length. A column already wider than the
-- target is left alone rather than narrowed, which could not succeed anyway if
-- any row were too long.
DO $$
DECLARE
    target record;
    current_len integer;
BEGIN
    FOR target IN
        SELECT * FROM (VALUES
            ('users',              'username',     255),
            ('principals',         'uri',          320),
            ('principals',         'email',        255),
            ('principals',         'displayname',  255),
            ('addressbooks',       'principaluri', 320),
            ('deleted_principals', 'principal',    320),
            ('deleted_principals', 'username',     255)
        ) AS v(tbl, col, len)
    LOOP
        SELECT a.atttypmod - 4 INTO current_len
          FROM pg_attribute a
         WHERE a.attrelid = to_regclass(target.tbl)
           AND a.attname = target.col
           AND a.attnum > 0 AND NOT a.attisdropped;

        IF current_len IS NULL THEN
            RAISE EXCEPTION 'expected column %.% does not exist', target.tbl, target.col;
        ELSIF current_len < target.len THEN
            EXECUTE format('ALTER TABLE %I ALTER COLUMN %I TYPE VARCHAR(%s)',
                           target.tbl, target.col, target.len);
            RAISE NOTICE 'widened %.% from varchar(%) to varchar(%)',
                         target.tbl, target.col, current_len, target.len;
        END IF;
    END LOOP;
END $$;
