-- calserver extension schema: tables sabre/dav's PDO backend does not define.
--
-- Sabre's CalDAV\Backend\PDO implements SharingSupport but deliberately leaves
-- NotificationSupport (the notification inbox + shareReply) and calendar
-- publishing unimplemented, so its schema has no home for them. calserver
-- fills those gaps here.
--
-- Every object below is a NEW table: a live sabre instance running against the
-- same database never references them, so loading this alongside the sabre
-- schema cannot affect it. No sabre-owned table is altered. Load this AFTER
-- the dav/examples/sql/pgsql.*.sql files.

-- Per-principal notification inbox (calendarserver invite notifications). One
-- row per delivered invite; entryid is the stable id that also names the
-- <principal>/notifications/<entryid>.xml resource.
CREATE TABLE notifications (
    id SERIAL NOT NULL,
    principaluri VARCHAR(100) NOT NULL,
    entryid VARCHAR(60) NOT NULL,
    dtstamp BIGINT NOT NULL,            -- unix seconds (BIGINT: Y2038-safe)
    hosturl TEXT NOT NULL,              -- shared calendar path (base-relative)
    organizer VARCHAR(100) NOT NULL,    -- sharer principal path
    organizername VARCHAR(100),
    access SMALLINT NOT NULL,           -- 2 = read, 3 = readwrite
    invitestatus SMALLINT NOT NULL,     -- 1 = noresponse, 2 = accepted, 3 = declined, 4 = invalid
    summary TEXT
);

ALTER TABLE ONLY notifications
    ADD CONSTRAINT notifications_pkey PRIMARY KEY (id);

CREATE UNIQUE INDEX notifications_principaluri_entryid
    ON notifications USING btree (principaluri, entryid);


-- Published calendars. A row exists exactly while a calendar is published;
-- publicuri is the opaque public path segment (a UUID, as sabre would mint).
-- The FK to calendars cascades on delete, so removing a calendar drops its
-- publication without the core DeleteCalendar path having to know this table
-- exists (keeping sabre-only databases valid).
CREATE TABLE calendarpublications (
    calendarid INTEGER NOT NULL REFERENCES calendars (id) ON DELETE CASCADE,
    publicuri VARCHAR(100) NOT NULL
);

ALTER TABLE ONLY calendarpublications
    ADD CONSTRAINT calendarpublications_pkey PRIMARY KEY (calendarid);


-- Pending account deletions (soft-delete tombstones). One row exists while an
-- account is scheduled for deletion but not yet purged. The auth plugin
-- rejects logins for a tombstoned principal (immediate disable), while its
-- data stays untouched until the retention window elapses and the purge job
-- (calctl principal purge-expired) runs DeleteUserCascade. purge_after is
-- materialized at schedule time so changing the retention config never moves
-- an already-scheduled purge. No FK to principals: the tombstone must survive
-- a manually-removed principal (the purge job tolerates that case).
CREATE TABLE deleted_principals (
    principal    VARCHAR(200) PRIMARY KEY,   -- "principals/alice"
    username     VARCHAR(50)  NOT NULL,      -- bare username, for the purge cascade
    deleted_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    purge_after  TIMESTAMPTZ  NOT NULL,      -- deleted_at + retention window
    reason       TEXT
);

CREATE INDEX deleted_principals_purge_after_ix
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
CREATE TABLE push_subscriptions (
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

CREATE UNIQUE INDEX push_subscriptions_token ON push_subscriptions (token);
CREATE INDEX push_subscriptions_lookup
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
CREATE TABLE jobs (
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
CREATE INDEX jobs_claim_ix   ON jobs (queue, state, scheduled_at);
-- Reclaim: orphaned 'executing' rows a dead worker never finished (D9 sweep).
CREATE INDEX jobs_reclaim_ix ON jobs (state, attempted_at);

-- Coalesce: at most one pending job per (uid, recipient, method). A newer
-- revision supersedes the pending one via ON CONFLICT DO UPDATE, ordered by
-- (SEQUENCE, DTSTAMP) from args (D10); an older/equal re-issue is a no-op. A
-- REQUEST's SEQUENCE is client-controlled (echoed, may be non-monotonic); a
-- server-generated CANCEL's is incremented (monotonic) — so SEQUENCE is an
-- ordering key with DTSTAMP as tiebreaker, never the identity. Scoped to
-- 'available' so a supersede racing an in-flight send inserts fresh. Rows
-- without these args keys (a future non-iMIP worker) have all-NULL keys, which
-- a unique index treats as distinct, so they never collide.
CREATE UNIQUE INDEX jobs_coalesce_ix ON jobs (
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
ALTER TABLE users      ALTER COLUMN username    TYPE VARCHAR(255);
ALTER TABLE principals ALTER COLUMN uri         TYPE VARCHAR(320);
ALTER TABLE principals ALTER COLUMN email       TYPE VARCHAR(255);
ALTER TABLE principals ALTER COLUMN displayname TYPE VARCHAR(255);


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
ALTER TABLE addressbooks       ALTER COLUMN principaluri TYPE VARCHAR(320);
ALTER TABLE deleted_principals ALTER COLUMN principal    TYPE VARCHAR(320);
ALTER TABLE deleted_principals ALTER COLUMN username     TYPE VARCHAR(255);
