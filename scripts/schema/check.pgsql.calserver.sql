-- Read-only audit of everything scripts/schema/pgsql.calserver.sql is
-- responsible for: the tables and indexes it creates, the index it drops, the
-- sabre columns it widens, and the columns of the tables it owns.
--
-- Nothing here writes or locks, so it is safe against a live database. Run it
-- to answer "has this file been applied, and is what is there still what the
-- code expects":
--
--     psql -d sabredav -f scripts/schema/check.pgsql.calserver.sql
--
-- Every row should say ok. Anything else is listed again in the summary at the
-- end, with a count — so a clean database prints "0 problems".
--
-- Why a separate file rather than just re-running the schema: applying the
-- schema is safe and idempotent, but this answers the question without changing
-- anything, which is what you want before touching production.

\pset footer off

WITH checks AS (
    -- Tables and indexes the file creates.
    SELECT 1 AS ord, 'relation' AS kind, v.name AS object,
           'exists' AS expected,
           CASE WHEN to_regclass(v.name) IS NULL THEN 'MISSING' ELSE 'exists' END AS found,
           to_regclass(v.name) IS NOT NULL AS ok
      FROM (VALUES
        ('notifications'), ('notifications_principaluri_entryid'),
        ('calendarpublications'),
        ('deleted_principals'), ('deleted_principals_purge_after_ix'),
        ('push_subscriptions'), ('push_subscriptions_token'), ('push_subscriptions_lookup'),
        ('jobs'), ('jobs_claim_ix'), ('jobs_reclaim_ix'), ('jobs_coalesce_ix'),
        ('jobs_latest_ix'), ('domain_scheduling_policy'),
        ('contact_frecency'), ('contact_frecency_rank_ix'),
        ('contact_import'), ('contact_import_run_ix'),
        ('calendar_availability'), ('free_busy_visibility')
      ) AS v(name)

    UNION ALL

    -- The over-strict sabre index the file drops. Expected to be ABSENT: while
    -- it exists, sharing a second calendar with the same person fails with a
    -- duplicate-key error.
    SELECT 2, 'dropped index', 'calendarinstances_principaluri_share_href',
           'absent',
           CASE WHEN to_regclass('calendarinstances_principaluri_share_href') IS NULL
                THEN 'absent' ELSE 'STILL PRESENT' END,
           to_regclass('calendarinstances_principaluri_share_href') IS NULL

    UNION ALL

    -- Columns widened to hold a full email address. Wider than asked for is
    -- fine; narrower means long-address accounts cannot provision, and the
    -- contacts import cannot create their address book.
    SELECT 3, 'column width', v.tbl || '.' || v.col,
           'varchar(' || v.len || ')',
           coalesce('varchar(' || w.len::text || ')', 'MISSING'),
           coalesce(w.len >= v.len, false)
      FROM (VALUES
        ('users',              'username',     255),
        ('principals',         'uri',          320),
        ('principals',         'email',        255),
        ('principals',         'displayname',  255),
        ('addressbooks',       'principaluri', 320),
        ('deleted_principals', 'principal',    320),
        ('deleted_principals', 'username',     255)
      ) AS v(tbl, col, len)
      LEFT JOIN LATERAL (
        SELECT a.atttypmod - 4 AS len FROM pg_attribute a
         WHERE a.attrelid = to_regclass(v.tbl) AND a.attname = v.col
           AND a.attnum > 0 AND NOT a.attisdropped
      ) w ON true

    UNION ALL

    -- Columns of the tables this file owns. A table created by an EARLIER
    -- version of the schema keeps its name, so CREATE TABLE IF NOT EXISTS
    -- skips it and the file reports success over a stale definition — these
    -- rows are how you see that.
    SELECT 4, 'column', v.tbl || '.' || v.col, 'exists',
           CASE WHEN a.attname IS NULL THEN 'MISSING' ELSE 'exists' END,
           a.attname IS NOT NULL
      FROM (VALUES
        ('contact_frecency', 'principal'), ('contact_frecency', 'email'),
        ('contact_frecency', 'name'),      ('contact_frecency', 'frec'),
        ('contact_frecency', 'seed_frec'), ('contact_frecency', 'last_used'),
        ('contact_import',   'principal'), ('contact_import',   'source_users'),
        ('contact_import',   'book_id'),   ('contact_import',   'book_uri'),
        ('contact_import',   'bundle_digest'), ('contact_import', 'exporter_ver'),
        ('contact_import',   'cards_total'),   ('contact_import', 'cards_written'),
        ('contact_import',   'cards_skipped'), ('contact_import', 'synctoken'),
        ('contact_import',   'run_id'),        ('contact_import', 'imported_at'),
        ('domain_scheduling_policy', 'domain'),
        ('domain_scheduling_policy', 'local_notify'),
        ('domain_scheduling_policy', 'share_notify'),
        ('domain_scheduling_policy', 'updated_at'),
        ('calendar_availability', 'principal'),
        ('calendar_availability', 'data'),
        ('calendar_availability', 'updated_at'),
        ('free_busy_visibility', 'principal'),
        ('free_busy_visibility', 'audience'),
        ('free_busy_visibility', 'allowed'),
        ('free_busy_visibility', 'updated_at')
      ) AS v(tbl, col)
      LEFT JOIN pg_attribute a
             ON a.attrelid = to_regclass(v.tbl) AND a.attname = v.col
            AND a.attnum > 0 AND NOT a.attisdropped
)
SELECT kind, object, expected, found,
       CASE WHEN ok THEN 'ok' ELSE '<<< FIX' END AS status
  FROM checks ORDER BY ord, object;

SELECT count(*) FILTER (WHERE NOT ok) AS problems,
       CASE WHEN count(*) FILTER (WHERE NOT ok) = 0
            THEN 'schema is up to date'
            ELSE 'apply scripts/schema/pgsql.calserver.sql (see the rows marked FIX above)'
       END AS verdict
  FROM (
    SELECT to_regclass(v.name) IS NOT NULL AS ok FROM (VALUES
      ('notifications'), ('notifications_principaluri_entryid'),
      ('calendarpublications'),
      ('deleted_principals'), ('deleted_principals_purge_after_ix'),
      ('push_subscriptions'), ('push_subscriptions_token'), ('push_subscriptions_lookup'),
      ('jobs'), ('jobs_claim_ix'), ('jobs_reclaim_ix'), ('jobs_coalesce_ix'),
      ('jobs_latest_ix'), ('domain_scheduling_policy'),
      ('contact_frecency'), ('contact_frecency_rank_ix'),
      ('contact_import'), ('contact_import_run_ix'),
      ('calendar_availability'), ('free_busy_visibility')) AS v(name)
    UNION ALL
    SELECT to_regclass('calendarinstances_principaluri_share_href') IS NULL
    UNION ALL
    SELECT coalesce((SELECT a.atttypmod - 4 FROM pg_attribute a
                      WHERE a.attrelid = to_regclass(v.tbl) AND a.attname = v.col
                        AND a.attnum > 0 AND NOT a.attisdropped) >= v.len, false)
      FROM (VALUES
        ('users','username',255), ('principals','uri',320),
        ('principals','email',255), ('principals','displayname',255),
        ('addressbooks','principaluri',320),
        ('deleted_principals','principal',320), ('deleted_principals','username',255)
      ) AS v(tbl, col, len)
    UNION ALL
    SELECT EXISTS (SELECT 1 FROM pg_attribute a
                    WHERE a.attrelid = to_regclass(v.tbl) AND a.attname = v.col
                      AND a.attnum > 0 AND NOT a.attisdropped)
      FROM (VALUES
        ('contact_frecency','principal'), ('contact_frecency','email'),
        ('contact_frecency','name'), ('contact_frecency','frec'),
        ('contact_frecency','seed_frec'), ('contact_frecency','last_used'),
        ('contact_import','principal'), ('contact_import','source_users'),
        ('contact_import','book_id'), ('contact_import','book_uri'),
        ('contact_import','bundle_digest'), ('contact_import','exporter_ver'),
        ('contact_import','cards_total'), ('contact_import','cards_written'),
        ('contact_import','cards_skipped'), ('contact_import','synctoken'),
        ('contact_import','run_id'), ('contact_import','imported_at'),
        ('domain_scheduling_policy','domain'), ('domain_scheduling_policy','local_notify'),
        ('domain_scheduling_policy','share_notify'), ('domain_scheduling_policy','updated_at'),
        ('calendar_availability','principal'), ('calendar_availability','data'),
        ('calendar_availability','updated_at'),
        ('free_busy_visibility','principal'), ('free_busy_visibility','audience'),
        ('free_busy_visibility','allowed'), ('free_busy_visibility','updated_at')
      ) AS v(tbl, col)
  ) AS all_checks(ok);
