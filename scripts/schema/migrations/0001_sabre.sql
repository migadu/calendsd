-- The tables sabre/dav's PDO backend defines, which calendsd still stores its
-- data in. Taken from sabre's dav/examples/sql/pgsql.*.sql, with two changes:
--
--   * Every statement is conditional (IF NOT EXISTS, primary keys inline under
--     sabre's constraint names), so this runs as a no-op on a database that
--     sabre created, and the runner can record it as applied there.
--   * sabre's seed rows (an 'admin' user with password "admin") are left out.
--     A production database must not get a default account.
--
-- calendarinstances_principaluri_share_href is not created: 0002 drops it
-- (see there for why), so a fresh database ends up like a migrated one.
--
-- The column widths are sabre's. 0002 widens the ones that hold an address.

CREATE TABLE IF NOT EXISTS addressbooks (
    id SERIAL NOT NULL,
    principaluri VARCHAR(255),
    displayname VARCHAR(255),
    uri VARCHAR(200),
    description TEXT,
    synctoken INTEGER NOT NULL DEFAULT 1,
    CONSTRAINT addressbooks_pkey PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS addressbooks_ukey
    ON addressbooks USING btree (principaluri, uri);

CREATE TABLE IF NOT EXISTS cards (
    id SERIAL NOT NULL,
    addressbookid INTEGER NOT NULL,
    carddata BYTEA,
    uri VARCHAR(200),
    lastmodified INTEGER,
    etag VARCHAR(32),
    size INTEGER NOT NULL,
    CONSTRAINT cards_pkey PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS cards_ukey
    ON cards USING btree (addressbookid, uri);

CREATE TABLE IF NOT EXISTS addressbookchanges (
    id SERIAL NOT NULL,
    uri VARCHAR(200) NOT NULL,
    synctoken INTEGER NOT NULL,
    addressbookid INTEGER NOT NULL,
    operation SMALLINT NOT NULL,
    CONSTRAINT addressbookchanges_pkey PRIMARY KEY (id)
);
CREATE INDEX IF NOT EXISTS addressbookchanges_addressbookid_synctoken_ix
    ON addressbookchanges USING btree (addressbookid, synctoken);

CREATE TABLE IF NOT EXISTS calendarobjects (
    id SERIAL NOT NULL,
    calendardata BYTEA,
    uri VARCHAR(200),
    calendarid INTEGER NOT NULL,
    lastmodified INTEGER,
    etag VARCHAR(32),
    size INTEGER NOT NULL,
    componenttype VARCHAR(8),
    firstoccurence INTEGER,
    lastoccurence INTEGER,
    uid VARCHAR(200),
    CONSTRAINT calendarobjects_pkey PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS calendarobjects_ukey
    ON calendarobjects USING btree (calendarid, uri);

CREATE TABLE IF NOT EXISTS calendars (
    id SERIAL NOT NULL,
    synctoken INTEGER NOT NULL DEFAULT 1,
    components VARCHAR(21),
    CONSTRAINT calendars_pkey PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS calendarinstances (
    id SERIAL NOT NULL,
    calendarid INTEGER NOT NULL,
    principaluri VARCHAR(100),
    access SMALLINT NOT NULL DEFAULT '1', -- '1 = owner, 2 = read, 3 = readwrite'
    displayname VARCHAR(100),
    uri VARCHAR(200),
    description TEXT,
    calendarorder INTEGER NOT NULL DEFAULT 0,
    calendarcolor VARCHAR(10),
    timezone TEXT,
    transparent SMALLINT NOT NULL DEFAULT '0',
    share_href VARCHAR(100),
    share_displayname VARCHAR(100),
    share_invitestatus SMALLINT NOT NULL DEFAULT '2', --  '1 = noresponse, 2 = accepted, 3 = declined, 4 = invalid'
    CONSTRAINT calendarinstances_pkey PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS calendarinstances_principaluri_uri
    ON calendarinstances USING btree (principaluri, uri);
CREATE UNIQUE INDEX IF NOT EXISTS calendarinstances_principaluri_calendarid
    ON calendarinstances USING btree (principaluri, calendarid);

CREATE TABLE IF NOT EXISTS calendarsubscriptions (
    id SERIAL NOT NULL,
    uri VARCHAR(200) NOT NULL,
    principaluri VARCHAR(100) NOT NULL,
    source TEXT,
    displayname VARCHAR(100),
    refreshrate VARCHAR(10),
    calendarorder INTEGER NOT NULL DEFAULT 0,
    calendarcolor VARCHAR(10),
    striptodos SMALLINT NULL,
    stripalarms SMALLINT NULL,
    stripattachments SMALLINT NULL,
    lastmodified INTEGER,
    CONSTRAINT calendarsubscriptions_pkey PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS calendarsubscriptions_ukey
    ON calendarsubscriptions USING btree (principaluri, uri);

CREATE TABLE IF NOT EXISTS calendarchanges (
    id SERIAL NOT NULL,
    uri VARCHAR(200) NOT NULL,
    synctoken INTEGER NOT NULL,
    calendarid INTEGER NOT NULL,
    operation SMALLINT NOT NULL DEFAULT 0,
    CONSTRAINT calendarchanges_pkey PRIMARY KEY (id)
);
CREATE INDEX IF NOT EXISTS calendarchanges_calendarid_synctoken_ix
    ON calendarchanges USING btree (calendarid, synctoken);

CREATE TABLE IF NOT EXISTS schedulingobjects (
    id SERIAL NOT NULL,
    principaluri VARCHAR(255),
    calendardata BYTEA,
    uri VARCHAR(200),
    lastmodified INTEGER,
    etag VARCHAR(32),
    size INTEGER NOT NULL,
    CONSTRAINT schedulingobjects_pkey PRIMARY KEY (id)
);

CREATE TABLE IF NOT EXISTS locks (
    id SERIAL NOT NULL,
    owner VARCHAR(100),
    timeout INTEGER,
    created INTEGER,
    token VARCHAR(100),
    scope SMALLINT,
    depth SMALLINT,
    uri TEXT,
    CONSTRAINT locks_pkey PRIMARY KEY (id)
);
CREATE INDEX IF NOT EXISTS locks_token_ix
    ON locks USING btree (token);
CREATE INDEX IF NOT EXISTS locks_uri_ix
    ON locks USING btree (uri);

CREATE TABLE IF NOT EXISTS principals (
    id SERIAL NOT NULL,
    uri VARCHAR(200) NOT NULL,
    email VARCHAR(80),
    displayname VARCHAR(80),
    CONSTRAINT principals_pkey PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS principals_ukey
    ON principals USING btree (uri);

CREATE TABLE IF NOT EXISTS groupmembers (
    id SERIAL NOT NULL,
    principal_id INTEGER NOT NULL,
    member_id INTEGER NOT NULL,
    CONSTRAINT groupmembers_pkey PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS groupmembers_ukey
    ON groupmembers USING btree (principal_id, member_id);

CREATE TABLE IF NOT EXISTS propertystorage (
    id SERIAL NOT NULL,
    path VARCHAR(1024) NOT NULL,
    name VARCHAR(100) NOT NULL,
    valuetype INT,
    value BYTEA,
    CONSTRAINT propertystorage_pkey PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS propertystorage_ukey
    ON propertystorage (path, name);

CREATE TABLE IF NOT EXISTS users (
    id SERIAL NOT NULL,
    username VARCHAR(50),
    digesta1 VARCHAR(32),
    CONSTRAINT users_pkey PRIMARY KEY (id)
);
CREATE UNIQUE INDEX IF NOT EXISTS users_ukey
    ON users USING btree (username);
