-- Accounts CalDAVTester expects, mapped onto the sabre schema.
--
-- user01..user10 + admin (password == username, realm "SabreDAV"; digesta1 is
-- md5("<user>:SabreDAV:<password>"), computed in-DB), plus Calendar-Server
-- style resource01..20 / location01..10 principals (phase 7: auto-accept
-- scheduling targets) and group01..40 principals (membership via
-- groupmembers). Principal URI is principals/<name>; calendar home resolves to
-- /calendars/<name>/, addressbook home to /addressbooks/<name>/ (see
-- serverinfo-overrides.py for the mapping).
--
-- The suite assumes a Calendar-Server-style auto-provisioned default
-- calendar/addressbook that regenerates after DELETEALL. calendsd (like sabre)
-- never auto-recreates collections, so these must exist up front and be
-- re-seeded between destructive test files (the runners do this). Every insert
-- is idempotent so re-seeding is cheap and safe.
DO $$
DECLARE i int; u text; names text[] := '{}';
BEGIN
  FOR i IN 1..10 LOOP names := names || ('user' || lpad(i::text, 2, '0')); END LOOP;
  names := names || 'admin'::text;
  FOR i IN 1..20 LOOP names := names || ('resource' || lpad(i::text, 2, '0')); END LOOP;
  FOR i IN 1..10 LOOP names := names || ('location' || lpad(i::text, 2, '0')); END LOOP;

  FOREACH u IN ARRAY names LOOP
    INSERT INTO principals (uri, email, displayname)
      VALUES ('principals/'||u, u||'@example.org', initcap(regexp_replace(u, '([a-z])([0-9])', '\1 \2')))
      ON CONFLICT (uri) DO NOTHING;
    -- calendar-proxy sub-principals, like sabre's examples/sql/pgsql.principals.sql;
    -- SetGroupMemberSet needs these rows to store delegates.
    INSERT INTO principals (uri) VALUES
      ('principals/'||u||'/calendar-proxy-read'),
      ('principals/'||u||'/calendar-proxy-write')
      ON CONFLICT (uri) DO NOTHING;
    INSERT INTO users (username, digesta1)
      VALUES (u, md5(u||':SabreDAV:'||u)) ON CONFLICT (username) DO NOTHING;

    IF NOT EXISTS (SELECT 1 FROM calendarinstances WHERE principaluri='principals/'||u AND uri='calendar') THEN
      INSERT INTO calendars (synctoken, components) VALUES (1, 'VEVENT,VTODO');
      INSERT INTO calendarinstances (calendarid, principaluri, access, displayname, uri, description, calendarorder, transparent)
        SELECT currval('calendars_id_seq'), 'principals/'||u, 1, 'calendar', 'calendar', '', 0, 0;
    END IF;

    -- tasks + addressbook only for the interactive user accounts.
    IF u NOT LIKE 'resource%' AND u NOT LIKE 'location%' THEN
      IF NOT EXISTS (SELECT 1 FROM calendarinstances WHERE principaluri='principals/'||u AND uri='tasks') THEN
        INSERT INTO calendars (synctoken, components) VALUES (1, 'VTODO');
        INSERT INTO calendarinstances (calendarid, principaluri, access, displayname, uri, description, calendarorder, transparent)
          SELECT currval('calendars_id_seq'), 'principals/'||u, 1, 'tasks', 'tasks', '', 0, 0;
      END IF;
      IF NOT EXISTS (SELECT 1 FROM addressbooks WHERE principaluri='principals/'||u AND uri='addressbook') THEN
        INSERT INTO addressbooks (principaluri, displayname, uri, description, synctoken)
          VALUES ('principals/'||u, 'addressbook', 'addressbook', '', 1);
      END IF;
    END IF;
  END LOOP;

  -- Group principals: no login, no calendars; membership lives in groupmembers.
  FOR i IN 1..40 LOOP
    u := 'group' || lpad(i::text, 2, '0');
    INSERT INTO principals (uri, email, displayname)
      VALUES ('principals/'||u, u||'@example.org', 'Group '||lpad(i::text, 2, '0'))
      ON CONFLICT (uri) DO NOTHING;
  END LOOP;

  -- Group memberships from Calendar Server's stock test directory that the
  -- group-attendee tests rely on: group02 = {user06, user07},
  -- group20 = {user02, user03} (the "changing membership" group's start
  -- state).
  INSERT INTO groupmembers (principal_id, member_id)
    SELECT grp.id, mem.id FROM principals grp, principals mem
    WHERE (grp.uri = 'principals/group02' AND mem.uri IN ('principals/user06','principals/user07'))
       OR (grp.uri = 'principals/group20' AND mem.uri IN ('principals/user02','principals/user03'))
    ON CONFLICT (principal_id, member_id) DO NOTHING;

  -- Calendar Server's stock test directory delegates every resource and
  -- location to user01 (write proxy); calendaruserproxy.xml's "Default
  -- setup" test depends on it.
  INSERT INTO groupmembers (principal_id, member_id)
    SELECT grp.id, mem.id FROM principals grp, principals mem
    WHERE mem.uri = 'principals/user01'
      AND grp.uri ~ '^principals/(resource|location)[0-9]+/calendar-proxy-write$'
    ON CONFLICT (principal_id, member_id) DO NOTHING;
END $$;
