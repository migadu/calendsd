-- Accounts CalDAVTester expects, mapped onto the sabre schema.
--
-- user01/user02, password == username, realm "SabreDAV". digesta1 is
-- md5("<user>:SabreDAV:<password>"). Principal URI is principals/<user>;
-- calendar home resolves to /calendars/<user>/, addressbook home to
-- /addressbooks/<user>/ (see serverinfo-overrides.py for the path mapping).
--
-- The suite assumes a Calendar-Server-style auto-provisioned default
-- calendar/addressbook that regenerates after DELETEALL. calserver (like
-- sabre) never auto-recreates collections, so these must exist up front and
-- be re-seeded between destructive test files (run.sh does this).

INSERT INTO principals (uri,email,displayname) VALUES
 ('principals/user01','user01@example.org','User 01'),
 ('principals/user02','user02@example.org','User 02')
ON CONFLICT (uri) DO NOTHING;

INSERT INTO users (username,digesta1) VALUES
 ('user01','dcdc647f81b0ba633b51e51488f6e98a'),
 ('user02','a0b2e857a22c593f5377b2a49143f8d3')
ON CONFLICT (username) DO NOTHING;

-- One event calendar ('calendar') and one tasks calendar ('tasks') per user.
DO $$
DECLARE u text;
BEGIN
  FOREACH u IN ARRAY ARRAY['principals/user01','principals/user02'] LOOP
    IF NOT EXISTS (SELECT 1 FROM calendarinstances WHERE principaluri=u AND uri='calendar') THEN
      INSERT INTO calendars (synctoken, components) VALUES (1,'VEVENT,VTODO');
      INSERT INTO calendarinstances (calendarid, principaluri, access, displayname, uri, description, calendarorder, transparent)
        SELECT currval('calendars_id_seq'), u, 1, 'calendar', 'calendar', '', 0, 0;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM calendarinstances WHERE principaluri=u AND uri='tasks') THEN
      INSERT INTO calendars (synctoken, components) VALUES (1,'VTODO');
      INSERT INTO calendarinstances (calendarid, principaluri, access, displayname, uri, description, calendarorder, transparent)
        SELECT currval('calendars_id_seq'), u, 1, 'tasks', 'tasks', '', 0, 0;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM addressbooks WHERE principaluri=u AND uri='addressbook') THEN
      INSERT INTO addressbooks (principaluri, displayname, uri, description, synctoken)
        VALUES (u, 'addressbook', 'addressbook', '', 1);
    END IF;
  END LOOP;
END $$;
