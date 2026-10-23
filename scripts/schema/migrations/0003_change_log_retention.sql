-- Change-log retention ([retention] change_log_days, storage.PruneChangeLogs).
--
-- created dates each change row, so old rows can be pruned. Rows that exist
-- when this runs get the migration time: nothing is pruned until a full
-- retention period after the deploy. now() is not volatile, so Postgres stores
-- the default in the catalog instead of rewriting the table; each ALTER holds
-- its ACCESS EXCLUSIVE lock only for that catalog change.
--
-- synctoken_floor is the lowest sync token that can still be answered with a
-- delta: every change row below it may have been pruned. A sync-collection
-- REPORT with a lower token gets {DAV:}valid-sync-token, and the client
-- resyncs in full.

ALTER TABLE calendarchanges ADD COLUMN created timestamptz NOT NULL DEFAULT now();
ALTER TABLE addressbookchanges ADD COLUMN created timestamptz NOT NULL DEFAULT now();
ALTER TABLE calendars ADD COLUMN synctoken_floor integer NOT NULL DEFAULT 0;
ALTER TABLE addressbooks ADD COLUMN synctoken_floor integer NOT NULL DEFAULT 0;
