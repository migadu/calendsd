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
