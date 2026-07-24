-- Accounts CalDAVTester expects, mapped onto the sabre schema.
--
-- user01..user10, password == username, realm "SabreDAV". digesta1 is
-- md5("<user>:SabreDAV:<password>"), computed in-DB. Principal URI is
-- principals/<user>; calendar home resolves to /calendars/<user>/, addressbook
-- home to /addressbooks/<user>/ (see serverinfo-overrides.py for the mapping).
--
-- The suite assumes a Calendar-Server-style auto-provisioned default
-- calendar/addressbook that regenerates after DELETEALL. calserver (like sabre)
-- never auto-recreates collections, so these must exist up front and be
-- re-seeded between destructive test files (the runners do this). Every insert
-- is idempotent so re-seeding is cheap and safe.
DO $$
DECLARE i int; u text;
BEGIN
  FOR i IN 1..10 LOOP
    u := 'user' || lpad(i::text, 2, '0');
    INSERT INTO principals (uri, email, displayname)
      VALUES ('principals/'||u, u||'@example.org', 'User '||lpad(i::text, 2, '0'))
      ON CONFLICT (uri) DO NOTHING;
    INSERT INTO users (username, digesta1)
      VALUES (u, md5(u||':SabreDAV:'||u)) ON CONFLICT (username) DO NOTHING;

    IF NOT EXISTS (SELECT 1 FROM calendarinstances WHERE principaluri='principals/'||u AND uri='calendar') THEN
      INSERT INTO calendars (synctoken, components) VALUES (1, 'VEVENT,VTODO');
      INSERT INTO calendarinstances (calendarid, principaluri, access, displayname, uri, description, calendarorder, transparent)
        SELECT currval('calendars_id_seq'), 'principals/'||u, 1, 'calendar', 'calendar', '', 0, 0;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM calendarinstances WHERE principaluri='principals/'||u AND uri='tasks') THEN
      INSERT INTO calendars (synctoken, components) VALUES (1, 'VTODO');
      INSERT INTO calendarinstances (calendarid, principaluri, access, displayname, uri, description, calendarorder, transparent)
        SELECT currval('calendars_id_seq'), 'principals/'||u, 1, 'tasks', 'tasks', '', 0, 0;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM addressbooks WHERE principaluri='principals/'||u AND uri='addressbook') THEN
      INSERT INTO addressbooks (principaluri, displayname, uri, description, synctoken)
        VALUES ('principals/'||u, 'addressbook', 'addressbook', '', 1);
    END IF;
  END LOOP;
END $$;
